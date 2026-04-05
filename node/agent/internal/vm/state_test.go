package vm

import (
	"encoding/json"
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

func TestIsMigrated(t *testing.T) {
	dir := t.TempDir()

	if IsMigrated(dir) {
		t.Error("IsMigrated should be false without marker")
	}

	SetMigrated(dir, true)
	if !IsMigrated(dir) {
		t.Error("IsMigrated should be true after SetMigrated(true)")
	}

	// DeriveStatus should return "migrated"
	if status := DeriveStatus(dir); status != "migrated" {
		t.Errorf("DeriveStatus = %q, want migrated", status)
	}

	SetMigrated(dir, false)
	if IsMigrated(dir) {
		t.Error("IsMigrated should be false after SetMigrated(false)")
	}
}

func TestDeriveStatus_MigratedBeforeStopped(t *testing.T) {
	// When both migrated marker and no running process, "migrated" should win
	dir := t.TempDir()
	SetMigrated(dir, true)
	// No PID file, no process — would be "stopped" without the marker
	if status := DeriveStatus(dir); status != "migrated" {
		t.Errorf("DeriveStatus = %q, want migrated (marker present, VM not running)", status)
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

func TestVMState_TailscaleAuthkey_Persistence(t *testing.T) {
	dir := t.TempDir()
	st := &VMState{
		Name:             "ts-test",
		VCPU:             2,
		RAMMIB:           2048,
		Mark:             999,
		Mode:             "normal",
		TailscaleAuthkey: "tskey-auth-fake-key-12345",
	}
	if err := SaveVMState(dir, st); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadVMState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TailscaleAuthkey != "tskey-auth-fake-key-12345" {
		t.Errorf("TailscaleAuthkey = %q, want tskey-auth-fake-key-12345", loaded.TailscaleAuthkey)
	}
}

func TestVMState_TailscaleAuthkey_OmittedWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	st := &VMState{
		Name:   "no-ts-key",
		VCPU:   2,
		RAMMIB: 2048,
		Mark:   888,
		Mode:   "normal",
	}
	if err := SaveVMState(dir, st); err != nil {
		t.Fatal(err)
	}

	// Verify the JSON doesn't include the key when empty
	data, _ := os.ReadFile(filepath.Join(dir, "vm.json"))
	var raw map[string]interface{}
	json.Unmarshal(data, &raw)
	if _, exists := raw["tailscale_authkey"]; exists {
		t.Error("tailscale_authkey should be omitted from JSON when empty")
	}

	// Verify it loads back as empty
	loaded, err := LoadVMState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TailscaleAuthkey != "" {
		t.Errorf("TailscaleAuthkey = %q, want empty", loaded.TailscaleAuthkey)
	}
}

func TestVMState_BackwardsCompat_NoAuthkey(t *testing.T) {
	// Simulate loading a vm.json from before the authkey field existed
	dir := t.TempDir()
	oldJSON := `{"name":"old-vm","vcpu":2,"ram_mib":2048,"mark":777,"mode":"normal","tailscale_ip":"100.1.2.3"}`
	os.WriteFile(filepath.Join(dir, "vm.json"), []byte(oldJSON), 0644)

	loaded, err := LoadVMState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TailscaleAuthkey != "" {
		t.Errorf("Old vm.json should have empty TailscaleAuthkey, got %q", loaded.TailscaleAuthkey)
	}
	if loaded.TailscaleIP != "100.1.2.3" {
		t.Errorf("TailscaleIP = %q, want 100.1.2.3", loaded.TailscaleIP)
	}
}

func TestReadPID_NoPIDFile(t *testing.T) {
	dir := t.TempDir()
	if pid := ReadPID(dir); pid != 0 {
		t.Errorf("ReadPID with no PID file = %d, want 0", pid)
	}
}
