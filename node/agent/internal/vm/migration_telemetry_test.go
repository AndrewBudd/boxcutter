package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrationTelemetry(t *testing.T) {
	tmpDir := t.TempDir()

	telem := NewMigrationTelemetry("test-vm", "node-1", "node-2", "firecracker", 2048)

	// Simulate phases
	telem.StartPhase("pre-sync")
	time.Sleep(10 * time.Millisecond)
	telem.EndPhase("pre-sync", 1073741824) // 1GB

	telem.StartPhase("pause")
	time.Sleep(5 * time.Millisecond)
	telem.EndPhase("pause", 0)

	telem.StartPhase("snapshot")
	time.Sleep(10 * time.Millisecond)
	telem.EndPhase("snapshot", 2147483648) // 2GB

	telem.StartPhase("transfer")
	time.Sleep(10 * time.Millisecond)
	telem.EndPhase("transfer", 2147483648)

	telem.StartPhase("import")
	time.Sleep(5 * time.Millisecond)
	telem.EndPhase("import", 0)

	telem.StartPhase("verify")
	time.Sleep(5 * time.Millisecond)
	telem.EndPhase("verify", 0)

	telem.Complete(100)
	telem.Write(tmpDir)

	// Read and verify
	data, err := os.ReadFile(filepath.Join(tmpDir, "migration.log"))
	if err != nil {
		t.Fatalf("migration.log not written: %v", err)
	}

	var log MigrationLog
	if err := json.Unmarshal(data, &log); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if log.VM != "test-vm" {
		t.Errorf("VM = %q, want test-vm", log.VM)
	}
	if log.Source != "node-1" {
		t.Errorf("Source = %q, want node-1", log.Source)
	}
	if log.Target != "node-2" {
		t.Errorf("Target = %q, want node-2", log.Target)
	}
	if log.Type != "firecracker" {
		t.Errorf("Type = %q, want firecracker", log.Type)
	}
	if log.RAMMiB != 2048 {
		t.Errorf("RAMMiB = %d, want 2048", log.RAMMiB)
	}
	if log.Result != "success" {
		t.Errorf("Result = %q, want success", log.Result)
	}
	if log.DowntimeMs != 100 {
		t.Errorf("DowntimeMs = %d, want 100", log.DowntimeMs)
	}
	if len(log.Phases) != 6 {
		t.Fatalf("got %d phases, want 6", len(log.Phases))
	}

	expectedPhases := []string{"pre-sync", "pause", "snapshot", "transfer", "import", "verify"}
	for i, phase := range log.Phases {
		if phase.Phase != expectedPhases[i] {
			t.Errorf("phase[%d] = %q, want %q", i, phase.Phase, expectedPhases[i])
		}
		if phase.Status != "ok" {
			t.Errorf("phase[%d] status = %q, want ok", i, phase.Status)
		}
		if phase.DurationMs < 0 {
			t.Errorf("phase[%d] duration = %d, want >= 0", i, phase.DurationMs)
		}
	}

	// Check bytes on data phases
	if log.Phases[0].Bytes != 1073741824 {
		t.Errorf("pre-sync bytes = %d, want 1073741824", log.Phases[0].Bytes)
	}
	if log.Phases[2].Bytes != 2147483648 {
		t.Errorf("snapshot bytes = %d, want 2147483648", log.Phases[2].Bytes)
	}
}

func TestMigrationTelemetryFailure(t *testing.T) {
	tmpDir := t.TempDir()

	telem := NewMigrationTelemetry("fail-vm", "node-1", "node-2", "qemu", 10240)

	telem.StartPhase("pre-sync")
	telem.EndPhase("pre-sync", 500000)

	telem.StartPhase("state-load")
	telem.FailPhase("state-load", os.ErrDeadlineExceeded)
	telem.Fail(os.ErrDeadlineExceeded)

	telem.Write(tmpDir)

	data, err := os.ReadFile(filepath.Join(tmpDir, "migration.log"))
	if err != nil {
		t.Fatalf("migration.log not written: %v", err)
	}

	var log MigrationLog
	json.Unmarshal(data, &log)

	if log.Result != "failed" {
		t.Errorf("Result = %q, want failed", log.Result)
	}
	if log.Error == "" {
		t.Error("Error should be set on failure")
	}
	if len(log.Phases) != 2 {
		t.Fatalf("got %d phases, want 2", len(log.Phases))
	}
	if log.Phases[1].Status != "failed" {
		t.Errorf("phase[1] status = %q, want failed", log.Phases[1].Status)
	}
}

func TestCaptureNodeResources(t *testing.T) {
	snap := CaptureNodeResources(0)
	if snap.FreeRAMMiB <= 0 {
		t.Errorf("FreeRAMMiB = %d, want > 0", snap.FreeRAMMiB)
	}
	if snap.AvailRAMMiB <= 0 {
		t.Errorf("AvailRAMMiB = %d, want > 0", snap.AvailRAMMiB)
	}
	if snap.LoadAvg == "" {
		t.Error("LoadAvg should not be empty")
	}
}
