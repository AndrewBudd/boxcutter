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
		// Build locally from Dockerfile
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
func (m *Manager) buildLocal() error {
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

	var removed []string
	for _, ver := range m.Versions() {
		if ver == m.currentHead {
			continue // never remove head
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
