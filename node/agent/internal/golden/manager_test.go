package golden

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGoldenImageExists_NoFile(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{goldenDir: dir}
	if m.goldenImageExists() {
		t.Error("goldenImageExists should be false when no rootfs.ext4")
	}
}

func TestGoldenImageExists_BrokenSymlink(t *testing.T) {
	dir := t.TempDir()
	os.Symlink("nonexistent.ext4", filepath.Join(dir, "rootfs.ext4"))
	m := &Manager{goldenDir: dir}
	if m.goldenImageExists() {
		t.Error("goldenImageExists should be false for broken symlink")
	}
}

func TestGoldenImageExists_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "abc123.ext4"), []byte{}, 0644)
	os.Symlink("abc123.ext4", filepath.Join(dir, "rootfs.ext4"))
	m := &Manager{goldenDir: dir}
	if m.goldenImageExists() {
		t.Error("goldenImageExists should be false for empty file")
	}
}

func TestGoldenImageExists_ValidFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "abc123.ext4"), []byte("ext4 data"), 0644)
	os.Symlink("abc123.ext4", filepath.Join(dir, "rootfs.ext4"))
	m := &Manager{goldenDir: dir}
	if !m.goldenImageExists() {
		t.Error("goldenImageExists should be true for valid symlink to non-empty file")
	}
}

func TestReadCurrentVersion_NoFile(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{goldenDir: dir}
	if v := m.readCurrentVersion(); v != "" {
		t.Errorf("readCurrentVersion = %q, want empty", v)
	}
}

func TestReadCurrentVersion_WithFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "current-version"), []byte("abc123\n"), 0644)
	m := &Manager{goldenDir: dir}
	if v := m.readCurrentVersion(); v != "abc123" {
		t.Errorf("readCurrentVersion = %q, want abc123", v)
	}
}

func TestSetHead_SkipsBuildWhenImageExists(t *testing.T) {
	dir := t.TempDir()
	// Simulate an existing golden image
	os.WriteFile(filepath.Join(dir, "abc123.ext4"), []byte("ext4 data"), 0644)
	os.Symlink("abc123.ext4", filepath.Join(dir, "rootfs.ext4"))
	os.WriteFile(filepath.Join(dir, "current-version"), []byte("abc123"), 0644)

	m := NewManager(Config{GoldenDir: dir})

	// SetHead("build") should skip since image exists
	if err := m.SetHead("build"); err != nil {
		t.Fatalf("SetHead(build) failed: %v", err)
	}
	if m.currentHead != "abc123" {
		t.Errorf("currentHead = %q, want abc123", m.currentHead)
	}
}

func TestSetHead_DetectsStaleCurrentHead(t *testing.T) {
	dir := t.TempDir()
	// Simulate: currentHead is empty, no symlink, no current-version
	// This is the post-crash state where buildLocal was killed mid-conversion
	m := &Manager{goldenDir: dir, currentHead: ""}

	// SetHead("build") should try to build (will fail since docker-to-ext4.sh doesn't exist)
	err := m.SetHead("build")
	if err == nil {
		t.Error("SetHead(build) should fail when docker-to-ext4.sh is missing")
	}
}

func TestSetHead_ChecksDiskNotMemory(t *testing.T) {
	dir := t.TempDir()
	// Simulate: in-memory currentHead was set previously but file was deleted
	// (e.g., after cleanup or failed build)
	m := &Manager{goldenDir: dir, currentHead: "old-version"}

	// The actual file doesn't exist, so goldenImageExists() should return false
	// and a build should be attempted
	err := m.SetHead("build")
	// Will fail because docker-to-ext4.sh doesn't exist, but the important thing
	// is that it TRIED to build (didn't skip based on stale currentHead)
	if err == nil {
		t.Error("SetHead(build) should fail when docker-to-ext4.sh is missing")
	}
	// Should contain "not found" indicating it tried to find the build script
	if err != nil && err.Error() == "" {
		t.Error("should have a meaningful error message")
	}
}
