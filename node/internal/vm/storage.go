package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CreateSnapshot creates a dm-snapshot COW overlay on the golden image.
func CreateSnapshot(vmDir, goldenPath, diskSize string) (*SnapshotState, error) {
	// Get golden image size
	info, err := os.Stat(goldenPath)
	if err != nil {
		return nil, fmt.Errorf("stat golden image: %w", err)
	}
	goldenSize := info.Size()

	// Parse requested disk size
	diskBytes, err := parseSize(diskSize)
	if err != nil {
		diskBytes = goldenSize
	}
	if diskBytes < goldenSize {
		diskBytes = goldenSize
	}

	cowPath := filepath.Join(vmDir, "cow.img")

	// Create sparse COW file
	if err := run("truncate", "-s", fmt.Sprintf("%d", diskBytes), cowPath); err != nil {
		return nil, fmt.Errorf("creating cow image: %w", err)
	}

	name := filepath.Base(vmDir)
	dmName := "bc-" + name

	// Set up loop devices
	baseLoop, err := runOutput("losetup", "--find", "--show", "--read-only", goldenPath)
	if err != nil {
		return nil, fmt.Errorf("losetup base: %w", err)
	}

	cowLoop, err := runOutput("losetup", "--find", "--show", cowPath)
	if err != nil {
		run("losetup", "-d", baseLoop)
		return nil, fmt.Errorf("losetup cow: %w", err)
	}

	sectors := goldenSize / 512
	dmTable := fmt.Sprintf("0 %d snapshot %s %s P 8", sectors, baseLoop, cowLoop)

	cmd := fmt.Sprintf("echo '%s' | dmsetup create %s", dmTable, dmName)
	if err := runShell(cmd); err != nil {
		run("losetup", "-d", cowLoop)
		run("losetup", "-d", baseLoop)
		return nil, fmt.Errorf("dmsetup create: %w", err)
	}

	ss := &SnapshotState{
		BaseLoop: baseLoop,
		CowLoop:  cowLoop,
		DMName:   dmName,
	}

	// Resize filesystem if disk is larger than golden
	if diskBytes > goldenSize {
		run("e2fsck", "-f", "-y", "/dev/mapper/"+dmName)
		run("resize2fs", "/dev/mapper/"+dmName)
	}

	return ss, nil
}

// EnsureSnapshot re-creates the dm-snapshot if it's not active (after reboot).
func EnsureSnapshot(vmDir, goldenPath string) error {
	name := filepath.Base(vmDir)
	dmName := "bc-" + name

	if _, err := os.Stat("/dev/mapper/" + dmName); err == nil {
		return nil // Already active
	}

	cowPath := filepath.Join(vmDir, "cow.img")
	if _, err := os.Stat(cowPath); err != nil {
		return fmt.Errorf("cow image not found: %w", err)
	}

	info, err := os.Stat(goldenPath)
	if err != nil {
		return fmt.Errorf("stat golden image: %w", err)
	}

	baseLoop, err := runOutput("losetup", "--find", "--show", "--read-only", goldenPath)
	if err != nil {
		return fmt.Errorf("losetup base: %w", err)
	}

	cowLoop, err := runOutput("losetup", "--find", "--show", cowPath)
	if err != nil {
		run("losetup", "-d", baseLoop)
		return fmt.Errorf("losetup cow: %w", err)
	}

	sectors := info.Size() / 512
	dmTable := fmt.Sprintf("0 %d snapshot %s %s P 8", sectors, baseLoop, cowLoop)
	if err := runShell(fmt.Sprintf("echo '%s' | dmsetup create %s", dmTable, dmName)); err != nil {
		run("losetup", "-d", cowLoop)
		run("losetup", "-d", baseLoop)
		return fmt.Errorf("dmsetup create: %w", err)
	}

	if err := SaveSnapshotState(vmDir, &SnapshotState{
		BaseLoop: baseLoop,
		CowLoop:  cowLoop,
		DMName:   dmName,
	}); err != nil {
		return err
	}

	return nil
}

// CleanupSnapshot removes the dm-snapshot and loop devices.
func CleanupSnapshot(vmDir string) {
	name := filepath.Base(vmDir)
	dmName := "bc-" + name

	// Remove dm device with retries
	for i := 0; i < 5; i++ {
		if run("dmsetup", "remove", dmName) == nil {
			break
		}
		time.Sleep(time.Second)
	}

	ss, err := LoadSnapshotState(vmDir)
	if err != nil {
		return
	}
	run("losetup", "-d", ss.CowLoop)
	run("losetup", "-d", ss.BaseLoop)
	os.Remove(filepath.Join(vmDir, "snapshot.json"))
}

func runShell(cmd string) error {
	return run("bash", "-c", cmd)
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	multiplier := int64(1)
	if strings.HasSuffix(s, "G") {
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "G")
	} else if strings.HasSuffix(s, "M") {
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "M")
	} else if strings.HasSuffix(s, "T") {
		multiplier = 1024 * 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "T")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n * multiplier, nil
}
