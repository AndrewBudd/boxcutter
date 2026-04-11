package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLaunchQEMU_MissingToolsImg verifies that launchQEMU returns an error
// when tools.img is not present at the expected path. This is a regression
// test for issue #174 where missing tools.img was only a warning, causing
// QEMU VMs to launch without the tapegun daemon.
func TestLaunchQEMU_MissingToolsImg(t *testing.T) {
	// toolsImagePath is /var/lib/boxcutter/tools.img — skip if it exists
	// (e.g. on a real node), since we want to test the missing case.
	if _, err := os.Stat(toolsImagePath); err == nil {
		t.Skip("tools.img exists on this host; cannot test missing-tools-img path")
	}

	// Create a temp VM directory with a fake kernel so localKernel() succeeds.
	vmDir := t.TempDir()
	fakeKernel := filepath.Join(vmDir, "vmlinuz")
	if err := os.WriteFile(fakeKernel, []byte("fake-kernel"), 0644); err != nil {
		t.Fatal(err)
	}

	st := &VMState{
		Name:   "test-vm",
		VCPU:   1,
		RAMMIB: 256,
		TAP:    "tap-test",
		MAC:    "AA:FC:00:00:00:99",
	}

	_, err := launchQEMU(vmDir, st)
	if err == nil {
		t.Fatal("expected launchQEMU to fail when tools.img is missing, but it succeeded")
	}
	if !strings.Contains(err.Error(), "tools.img") {
		t.Fatalf("expected error to mention tools.img, got: %v", err)
	}
}

// TestLaunchQEMUIncoming_MissingToolsImg verifies that launchQEMUIncoming
// returns an error when tools.img is missing.
func TestLaunchQEMUIncoming_MissingToolsImg(t *testing.T) {
	if _, err := os.Stat(toolsImagePath); err == nil {
		t.Skip("tools.img exists on this host; cannot test missing-tools-img path")
	}

	vmDir := t.TempDir()
	fakeKernel := filepath.Join(vmDir, "vmlinuz")
	if err := os.WriteFile(fakeKernel, []byte("fake-kernel"), 0644); err != nil {
		t.Fatal(err)
	}
	// launchQEMUIncoming expects a rootfs file
	if err := os.WriteFile(filepath.Join(vmDir, "rootfs.ext4"), []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}

	st := &VMState{
		Name:   "test-vm",
		VCPU:   1,
		RAMMIB: 256,
		TAP:    "tap-test",
		MAC:    "AA:FC:00:00:00:99",
	}

	_, err := launchQEMUIncoming(vmDir, st)
	if err == nil {
		t.Fatal("expected launchQEMUIncoming to fail when tools.img is missing, but it succeeded")
	}
	if !strings.Contains(err.Error(), "tools.img") {
		t.Fatalf("expected error to mention tools.img, got: %v", err)
	}
}
