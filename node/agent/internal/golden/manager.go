// Package golden manages golden image versions on a node.
// It handles pulling new versions from OCI registries, switching
// the head version, and garbage collecting unused versions.
package golden

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultGoldenDir  = "/var/lib/boxcutter/golden"
	defaultOCIRegistry   = "ghcr.io"
	defaultOCIRepository = "AndrewBudd/boxcutter"
)

// Manager handles golden image lifecycle on a single node.
type Manager struct {
	goldenDir string
	registry  string
	repo      string

	mu          sync.Mutex
	currentHead string // the version currently set as head
}

// Config for the golden image manager.
type Config struct {
	GoldenDir     string // default: /var/lib/boxcutter/golden
	OCIRegistry   string // default: ghcr.io
	OCIRepository string // default: AndrewBudd/boxcutter
}

// NewManager creates a golden image manager.
func NewManager(cfg Config) *Manager {
	if cfg.GoldenDir == "" {
		cfg.GoldenDir = defaultGoldenDir
	}
	if cfg.OCIRegistry == "" {
		cfg.OCIRegistry = defaultOCIRegistry
	}
	if cfg.OCIRepository == "" {
		cfg.OCIRepository = defaultOCIRepository
	}

	m := &Manager{
		goldenDir: cfg.GoldenDir,
		registry:  cfg.OCIRegistry,
		repo:      cfg.OCIRepository,
	}

	// Read current head from symlink
	m.currentHead = m.readCurrentVersion()

	return m
}

// SetHead is called when the orchestrator publishes a new golden head version.
// If the version is "build", it builds locally from the Dockerfile.
// Otherwise, it pulls the image from OCI if not already present.
func (m *Manager) SetHead(version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if version == m.currentHead {
		return nil // already at this version
	}

	log.Printf("golden: head changing from %s to %s", m.currentHead, version)

	if version == "build" || version == "latest" {
		// Build locally from Dockerfile, but only if no golden image exists yet
		if m.currentHead != "" {
			log.Printf("golden: already have version %s, skipping build", m.currentHead)
			return nil
		}
		if err := m.buildLocal(); err != nil {
			return fmt.Errorf("building golden image locally: %w", err)
		}
		// Read the version that was just built
		version = m.readCurrentVersion()
		if version == "" {
			return fmt.Errorf("golden image built but no version found")
		}
	} else {
		// Check if we already have this version
		versionPath := filepath.Join(m.goldenDir, version+".ext4")
		if _, err := os.Stat(versionPath); err != nil {
			// Need to pull it
			log.Printf("golden: pulling version %s from OCI registry", version)
			if err := m.pullFromOCI(version); err != nil {
				return fmt.Errorf("pulling golden image %s: %w", version, err)
			}
		}

		// Update the symlink
		symlinkPath := filepath.Join(m.goldenDir, "rootfs.ext4")
		tmpLink := symlinkPath + ".tmp"
		os.Remove(tmpLink)
		if err := os.Symlink(version+".ext4", tmpLink); err != nil {
			return fmt.Errorf("creating symlink: %w", err)
		}
		if err := os.Rename(tmpLink, symlinkPath); err != nil {
			return fmt.Errorf("activating symlink: %w", err)
		}

		// Update current-version file
		os.WriteFile(filepath.Join(m.goldenDir, "current-version"), []byte(version), 0644)
	}

	m.currentHead = version
	log.Printf("golden: now using version %s", version)

	return nil
}

// buildLocal runs docker-to-ext4.sh to build the golden image from the Dockerfile.
// It first kills any orphaned build processes left from a previous agent (Bug #85:
// KillMode=process lets children survive agent restart, causing concurrent builds).
func (m *Manager) buildLocal() error {
	// Kill orphaned docker-to-ext4.sh processes from previous agent instances.
	// With KillMode=process, these survive agent restarts and race with new builds.
	m.killOrphanedBuilds()

	// An orphaned build may have completed — re-check disk before starting a new one.
	if ver := m.readCurrentVersion(); ver != "" {
		log.Printf("golden: orphaned build completed (version %s), skipping new build", ver)
		return nil
	}

	scriptPath := filepath.Join(m.goldenDir, "docker-to-ext4.sh")
	if _, err := os.Stat(scriptPath); err != nil {
		return fmt.Errorf("docker-to-ext4.sh not found at %s", scriptPath)
	}

	log.Printf("golden: building locally from Dockerfile")
	cmd := exec.Command("bash", scriptPath)
	cmd.Dir = m.goldenDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// killOrphanedBuilds finds and kills docker-to-ext4.sh processes that aren't
// children of this agent (i.e., orphans from previous agent instances).
func (m *Manager) killOrphanedBuilds() {
	myPID := os.Getpid()
	out, err := exec.Command("pgrep", "-f", "docker-to-ext4.sh").Output()
	if err != nil {
		return // no matching processes
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err != nil {
			continue
		}
		// Read the parent PID to check if it's an orphan (parent != us)
		ppidData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		// /proc/PID/stat format: pid (comm) state ppid ...
		// Find closing paren, then parse fields after it
		statStr := string(ppidData)
		closeIdx := strings.LastIndex(statStr, ")")
		if closeIdx < 0 || closeIdx+2 >= len(statStr) {
			continue
		}
		fields := strings.Fields(statStr[closeIdx+2:])
		if len(fields) < 2 {
			continue
		}
		var ppid int
		fmt.Sscanf(fields[1], "%d", &ppid)

		if ppid != myPID {
			log.Printf("golden: killing orphaned build process PID %d (parent %d, we are %d)", pid, ppid, myPID)
			// Kill the process group to also terminate child docker processes
			syscall.Kill(-pid, syscall.SIGTERM)
			syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	// Give processes a moment to die
	time.Sleep(2 * time.Second)
	// Force-kill any survivors
	out, err = exec.Command("pgrep", "-f", "docker-to-ext4.sh").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err != nil {
			continue
		}
		ppidData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		statStr := string(ppidData)
		closeIdx := strings.LastIndex(statStr, ")")
		if closeIdx < 0 || closeIdx+2 >= len(statStr) {
			continue
		}
		fields := strings.Fields(statStr[closeIdx+2:])
		if len(fields) < 2 {
			continue
		}
		var ppid int
		fmt.Sscanf(fields[1], "%d", &ppid)
		if ppid != myPID {
			log.Printf("golden: force-killing orphaned build PID %d", pid)
			syscall.Kill(-pid, syscall.SIGKILL)
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// CurrentHead returns the current golden head version.
func (m *Manager) CurrentHead() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentHead
}

// Versions returns all golden image versions on disk.
func (m *Manager) Versions() []string {
	entries, err := os.ReadDir(m.goldenDir)
	if err != nil {
		return nil
	}
	var versions []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".ext4") && !e.IsDir() && name != "rootfs.ext4" {
			ver := strings.TrimSuffix(name, ".ext4")
			versions = append(versions, ver)
		}
	}
	return versions
}

// GCUnused removes golden image versions that are not referenced by any VM
// and are not the current head. The inUse set contains versions still
// needed by running VMs.
func (m *Manager) GCUnused(inUse map[string]bool) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Also read current-version from disk in case in-memory state is stale
	// (e.g., build was killed before updating currentHead).
	diskHead := m.readCurrentVersion()

	var removed []string
	for _, ver := range m.Versions() {
		if ver == m.currentHead || ver == diskHead {
			continue // never remove head (in-memory or on-disk)
		}
		if inUse[ver] {
			continue // still in use by a VM
		}
		path := filepath.Join(m.goldenDir, ver+".ext4")
		if os.Remove(path) == nil {
			removed = append(removed, ver)
			log.Printf("golden: GC removed unused version %s", ver)
		}
	}
	return removed
}

// pullFromOCI downloads a golden image from the OCI registry using the oras CLI.
func (m *Manager) pullFromOCI(version string) error {
	os.MkdirAll(m.goldenDir, 0755)

	tmpDir, err := os.MkdirTemp("", "golden-pull-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	// Pull using oras CLI (installed on nodes)
	// OCI registries require lowercase repository names
	ref := fmt.Sprintf("%s/%s/golden:%s", m.registry, strings.ToLower(m.repo), version)
	log.Printf("golden: pulling %s", ref)

	cmd := exec.Command("oras", "pull", "--output", tmpDir, ref)
	cmd.Env = append(os.Environ(), "HOME=/tmp")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("oras pull: %w", err)
	}

	// Find the downloaded file (should be .zst)
	entries, _ := os.ReadDir(tmpDir)
	var srcFile string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".zst") {
			srcFile = filepath.Join(tmpDir, e.Name())
			break
		}
		if strings.HasSuffix(e.Name(), ".ext4") {
			srcFile = filepath.Join(tmpDir, e.Name())
			break
		}
	}
	if srcFile == "" {
		return fmt.Errorf("no image file found in pull output")
	}

	destPath := filepath.Join(m.goldenDir, version+".ext4")

	if strings.HasSuffix(srcFile, ".zst") {
		// Decompress to temp file, then sparsify to save disk space
		log.Printf("golden: decompressing %s", filepath.Base(srcFile))
		tmpDest := destPath + ".tmp"
		cmd := exec.Command("zstd", "-d", "-f", srcFile, "-o", tmpDest)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			os.Remove(tmpDest)
			return fmt.Errorf("zstd decompress: %w", err)
		}
		// Punch holes in zero regions to make the file sparse
		sparsify := exec.Command("fallocate", "--dig-holes", tmpDest)
		if err := sparsify.Run(); err != nil {
			log.Printf("golden: fallocate --dig-holes failed (non-fatal): %v", err)
		}
		if err := os.Rename(tmpDest, destPath); err != nil {
			os.Remove(tmpDest)
			return fmt.Errorf("renaming decompressed file: %w", err)
		}
	} else {
		// Just move it
		if err := os.Rename(srcFile, destPath); err != nil {
			// Cross-device? Copy instead.
			return copyFile(srcFile, destPath)
		}
	}

	log.Printf("golden: version %s ready at %s", version, destPath)
	return nil
}

func (m *Manager) readCurrentVersion() string {
	data, err := os.ReadFile(filepath.Join(m.goldenDir, "current-version"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// HashFile returns the first 12 chars of the SHA-256 hash of a file.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
