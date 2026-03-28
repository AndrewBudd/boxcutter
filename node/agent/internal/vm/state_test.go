package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIsFileRootfs_QCOW2(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "rootfs.qcow2"), []byte{}, 0644)

	if !IsFileRootfs(dir) {
		t.Error("IsFileRootfs should return true for rootfs.qcow2")
	}
}

func TestIsFileRootfs_Ext4(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "rootfs.ext4"), []byte{}, 0644)

	if !IsFileRootfs(dir) {
		t.Error("IsFileRootfs should return true for rootfs.ext4")
	}
}

func TestIsFileRootfs_Empty(t *testing.T) {
	dir := t.TempDir()

	if IsFileRootfs(dir) {
		t.Error("IsFileRootfs should return false for empty dir (dm-snapshot)")
	}
}

func TestIsFileRootfs_QCOW2Preferred(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "rootfs.qcow2"), []byte{}, 0644)
	os.WriteFile(filepath.Join(dir, "rootfs.ext4"), []byte{}, 0644)

	if !IsFileRootfs(dir) {
		t.Error("IsFileRootfs should return true when both exist")
	}
}

func TestRootfsPath_QCOW2First(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "rootfs.qcow2"), []byte{}, 0644)
	os.WriteFile(filepath.Join(dir, "rootfs.ext4"), []byte{}, 0644)

	got := RootfsPath(dir)
	want := filepath.Join(dir, "rootfs.qcow2")
	if got != want {
		t.Errorf("RootfsPath = %q, want %q (QCOW2 should take priority)", got, want)
	}
}

func TestRootfsPath_Ext4Fallback(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "rootfs.ext4"), []byte{}, 0644)

	got := RootfsPath(dir)
	want := filepath.Join(dir, "rootfs.ext4")
	if got != want {
		t.Errorf("RootfsPath = %q, want %q", got, want)
	}
}

func TestDiskFormat(t *testing.T) {
	tests := []struct {
		name     string
		files    []string
		expected string
	}{
		{"qcow2", []string{"rootfs.qcow2"}, "qcow2"},
		{"raw ext4", []string{"rootfs.ext4"}, "raw"},
		{"dm-snapshot", []string{}, "dm"},
		{"qcow2 wins over ext4", []string{"rootfs.qcow2", "rootfs.ext4"}, "qcow2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tt.files {
				os.WriteFile(filepath.Join(dir, f), []byte{}, 0644)
			}
			got := DiskFormat(dir)
			if got != tt.expected {
				t.Errorf("DiskFormat = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestIsMigrating(t *testing.T) {
	dir := t.TempDir()

	if IsMigrating(dir) {
		t.Error("IsMigrating should be false without marker")
	}

	os.WriteFile(filepath.Join(dir, "migrating"), []byte("192.168.50.11:8800"), 0644)

	if !IsMigrating(dir) {
		t.Error("IsMigrating should be true with marker")
	}
}

func TestMigrationTarget(t *testing.T) {
	dir := t.TempDir()

	// No marker
	if target := MigrationTarget(dir); target != "" {
		t.Errorf("MigrationTarget = %q, want empty", target)
	}

	// Modern marker with target address
	os.WriteFile(filepath.Join(dir, "migrating"), []byte("192.168.50.11:8800"), 0644)
	if target := MigrationTarget(dir); target != "192.168.50.11:8800" {
		t.Errorf("MigrationTarget = %q, want 192.168.50.11:8800", target)
	}
}

func TestReadPID_UsesOwnPID(t *testing.T) {
	dir := t.TempDir()
	myPID := os.Getpid() // use our own PID — guaranteed to be alive

	// QEMU PID file takes priority
	os.WriteFile(filepath.Join(dir, "qemu.pid"), []byte(fmt.Sprintf("%d\n", myPID)), 0644)
	if pid := ReadPID(dir); pid != myPID {
		t.Errorf("ReadPID = %d, want %d (from qemu.pid)", pid, myPID)
	}

	// Firecracker PID file fallback
	os.Remove(filepath.Join(dir, "qemu.pid"))
	os.WriteFile(filepath.Join(dir, "firecracker.pid"), []byte(fmt.Sprintf("%d", myPID)), 0644)
	if pid := ReadPID(dir); pid != myPID {
		t.Errorf("ReadPID = %d, want %d (from firecracker.pid)", pid, myPID)
	}
}

func TestReadPID_DeadProcess(t *testing.T) {
	dir := t.TempDir()
	// PID that definitely doesn't exist
	os.WriteFile(filepath.Join(dir, "qemu.pid"), []byte("999999999"), 0644)
	if pid := ReadPID(dir); pid != 0 {
		t.Errorf("ReadPID for dead PID = %d, want 0", pid)
	}
}

func TestReadPID_NoPIDFile(t *testing.T) {
	dir := t.TempDir()
	if pid := ReadPID(dir); pid != 0 {
		t.Errorf("ReadPID with no PID file = %d, want 0", pid)
	}
}
