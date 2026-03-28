package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBackendFor_QEMU(t *testing.T) {
	b := BackendFor("qemu")
	if _, ok := b.(*QEMUBackend); !ok {
		t.Errorf("BackendFor(\"qemu\") returned %T, want *QEMUBackend", b)
	}
}

func TestBackendFor_Firecracker(t *testing.T) {
	b := BackendFor("firecracker")
	if _, ok := b.(*FirecrackerBackend); !ok {
		t.Errorf("BackendFor(\"firecracker\") returned %T, want *FirecrackerBackend", b)
	}
}

func TestBackendFor_Empty(t *testing.T) {
	b := BackendFor("")
	if _, ok := b.(*FirecrackerBackend); !ok {
		t.Errorf("BackendFor(\"\") returned %T, want *FirecrackerBackend (default)", b)
	}
}

func TestBackendForVM_LoadsType(t *testing.T) {
	dir := t.TempDir()

	// QEMU VM
	st := VMState{Type: "qemu", Name: "test"}
	data, _ := json.Marshal(st)
	os.WriteFile(filepath.Join(dir, "vm.json"), data, 0644)

	b := BackendForVM(dir)
	if _, ok := b.(*QEMUBackend); !ok {
		t.Errorf("BackendForVM with type=qemu returned %T, want *QEMUBackend", b)
	}
}

func TestBackendForVM_MissingJSON(t *testing.T) {
	dir := t.TempDir()
	b := BackendForVM(dir)
	if _, ok := b.(*FirecrackerBackend); !ok {
		t.Errorf("BackendForVM with missing vm.json returned %T, want *FirecrackerBackend", b)
	}
}

func TestBackendForVM_NoType(t *testing.T) {
	dir := t.TempDir()
	st := VMState{Name: "test"}
	data, _ := json.Marshal(st)
	os.WriteFile(filepath.Join(dir, "vm.json"), data, 0644)

	b := BackendForVM(dir)
	if _, ok := b.(*FirecrackerBackend); !ok {
		t.Errorf("BackendForVM with empty type returned %T, want *FirecrackerBackend", b)
	}
}

func TestQEMUBackend_WriteConfig_Noop(t *testing.T) {
	q := &QEMUBackend{}
	dir := t.TempDir()
	st := &VMState{Name: "test"}

	if err := q.WriteConfig(dir, st); err != nil {
		t.Errorf("QEMUBackend.WriteConfig should be no-op, got error: %v", err)
	}

	// Verify no files were created
	entries, _ := os.ReadDir(dir)
	if len(entries) > 0 {
		t.Errorf("QEMUBackend.WriteConfig created files: %v", entries)
	}
}

func TestQEMUBackend_DiskName(t *testing.T) {
	q := &QEMUBackend{}

	// With QCOW2 present
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "rootfs.qcow2"), []byte{}, 0644)
	if got := q.DiskName(dir); got != "rootfs.qcow2" {
		t.Errorf("DiskName with qcow2 = %q, want rootfs.qcow2", got)
	}

	// Without QCOW2 (legacy)
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir2, "rootfs.ext4"), []byte{}, 0644)
	if got := q.DiskName(dir2); got != "rootfs.ext4" {
		t.Errorf("DiskName without qcow2 = %q, want rootfs.ext4", got)
	}
}

func TestFirecrackerBackend_DiskName(t *testing.T) {
	f := &FirecrackerBackend{}
	dir := t.TempDir()
	if got := f.DiskName(dir); got != "rootfs.ext4" {
		t.Errorf("FC DiskName = %q, want rootfs.ext4", got)
	}
}
