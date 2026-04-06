package vm

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MigrationPhase records timing and metadata for one phase of a migration.
type MigrationPhase struct {
	Phase      string `json:"phase"`
	Start      string `json:"start"`
	DurationMs int64  `json:"duration_ms"`
	Bytes      int64  `json:"bytes,omitempty"`
	Status     string `json:"status,omitempty"` // "ok" or "failed"
	Error      string `json:"error,omitempty"`
}

// ResourceSnapshot captures target node resource state.
type ResourceSnapshot struct {
	Timestamp    string `json:"timestamp"`
	FreeRAMMiB   int64  `json:"free_ram_mib"`
	AvailRAMMiB  int64  `json:"avail_ram_mib"`
	FreeDiskMB   int64  `json:"free_disk_mb,omitempty"`
	LoadAvg      string `json:"load_avg,omitempty"`
	QEMURSSMiB   int64  `json:"qemu_rss_mib,omitempty"`
	QEMUStatus   string `json:"qemu_status,omitempty"`
}

// MigrationLog is the structured telemetry record for a migration attempt.
type MigrationLog struct {
	VM              string             `json:"vm"`
	Source          string             `json:"source"`
	Target          string             `json:"target"`
	Type            string             `json:"type"` // "firecracker" or "qemu"
	RAMMiB          int                `json:"ram_mib"`
	StartTime       string             `json:"start_time"`
	Phases          []MigrationPhase   `json:"phases"`
	Resources       []ResourceSnapshot `json:"resources,omitempty"`
	QMPSnapshots    []json.RawMessage  `json:"qmp_snapshots,omitempty"`
	QEMUStderr      string             `json:"qemu_stderr,omitempty"`
	DmesgErrors     string             `json:"dmesg_errors,omitempty"`
	Result          string             `json:"result"` // "success" or "failed"
	Error           string             `json:"error,omitempty"`
	TotalDurationMs int64              `json:"total_duration_ms"`
	DowntimeMs      int64              `json:"downtime_ms,omitempty"`
}

// MigrationTelemetry tracks a migration in progress.
type MigrationTelemetry struct {
	log       *MigrationLog
	startTime time.Time
	phaseStart time.Time
}

// NewMigrationTelemetry creates a new telemetry tracker.
func NewMigrationTelemetry(vmName, source, target, vmType string, ramMiB int) *MigrationTelemetry {
	now := time.Now()
	return &MigrationTelemetry{
		log: &MigrationLog{
			VM:        vmName,
			Source:     source,
			Target:    target,
			Type:      vmType,
			RAMMiB:    ramMiB,
			StartTime: now.Format(time.RFC3339),
		},
		startTime: now,
	}
}

// StartPhase begins timing a new phase.
func (t *MigrationTelemetry) StartPhase(phase string) {
	t.phaseStart = time.Now()
	log.Printf("Migration %s: [%s] started", t.log.VM, phase)
}

// EndPhase records a completed phase.
func (t *MigrationTelemetry) EndPhase(phase string, bytes int64) {
	duration := time.Since(t.phaseStart)
	t.log.Phases = append(t.log.Phases, MigrationPhase{
		Phase:      phase,
		Start:      t.phaseStart.Format(time.RFC3339),
		DurationMs: duration.Milliseconds(),
		Bytes:      bytes,
		Status:     "ok",
	})
	if bytes > 0 {
		log.Printf("Migration %s: [%s] completed in %s (%dMB)",
			t.log.VM, phase, duration.Round(time.Millisecond), bytes/1024/1024)
	} else {
		log.Printf("Migration %s: [%s] completed in %s",
			t.log.VM, phase, duration.Round(time.Millisecond))
	}
}

// FailPhase records a failed phase.
func (t *MigrationTelemetry) FailPhase(phase string, err error) {
	duration := time.Since(t.phaseStart)
	t.log.Phases = append(t.log.Phases, MigrationPhase{
		Phase:      phase,
		Start:      t.phaseStart.Format(time.RFC3339),
		DurationMs: duration.Milliseconds(),
		Status:     "failed",
		Error:      err.Error(),
	})
	log.Printf("Migration %s: [%s] FAILED after %s: %v",
		t.log.VM, phase, duration.Round(time.Millisecond), err)
}

// AddResource captures a resource snapshot.
func (t *MigrationTelemetry) AddResource(snap ResourceSnapshot) {
	snap.Timestamp = time.Now().Format(time.RFC3339)
	t.log.Resources = append(t.log.Resources, snap)
}

// AddQMPSnapshot adds a QMP query-migrate response.
func (t *MigrationTelemetry) AddQMPSnapshot(data json.RawMessage) {
	t.log.QMPSnapshots = append(t.log.QMPSnapshots, data)
}

// SetQEMUStderr captures QEMU's stderr output.
func (t *MigrationTelemetry) SetQEMUStderr(stderr string) {
	t.log.QEMUStderr = strings.TrimSpace(stderr)
}

// SetDmesgErrors captures relevant dmesg errors.
func (t *MigrationTelemetry) SetDmesgErrors(errors string) {
	t.log.DmesgErrors = strings.TrimSpace(errors)
}

// Complete finalizes the log as successful.
func (t *MigrationTelemetry) Complete(downtimeMs int64) {
	t.log.Result = "success"
	t.log.TotalDurationMs = time.Since(t.startTime).Milliseconds()
	t.log.DowntimeMs = downtimeMs
}

// Fail finalizes the log as failed.
func (t *MigrationTelemetry) Fail(err error) {
	t.log.Result = "failed"
	t.log.Error = err.Error()
	t.log.TotalDurationMs = time.Since(t.startTime).Milliseconds()
}

// Write writes the telemetry log to the VM directory.
func (t *MigrationTelemetry) Write(vmDir string) {
	logPath := filepath.Join(vmDir, "migration.log")
	data, err := json.MarshalIndent(t.log, "", "  ")
	if err != nil {
		log.Printf("Migration %s: failed to marshal telemetry: %v", t.log.VM, err)
		return
	}
	if err := os.WriteFile(logPath, data, 0644); err != nil {
		log.Printf("Migration %s: failed to write telemetry to %s: %v", t.log.VM, logPath, err)
		return
	}
	log.Printf("Migration %s: telemetry written to %s", t.log.VM, logPath)
}

// CaptureNodeResources takes a snapshot of the local node's resources.
func CaptureNodeResources(qemuPID int) ResourceSnapshot {
	snap := ResourceSnapshot{}

	// Memory from /proc/meminfo
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "MemFree:") {
				fmt.Sscanf(line, "MemFree: %d kB", &snap.FreeRAMMiB)
				snap.FreeRAMMiB /= 1024
			}
			if strings.HasPrefix(line, "MemAvailable:") {
				fmt.Sscanf(line, "MemAvailable: %d kB", &snap.AvailRAMMiB)
				snap.AvailRAMMiB /= 1024
			}
		}
	}

	// Load average
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) >= 3 {
			snap.LoadAvg = strings.Join(parts[:3], " ")
		}
	}

	// QEMU process info
	if qemuPID > 0 {
		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", qemuPID)); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "VmRSS:") {
					var rssKB int64
					fmt.Sscanf(line, "VmRSS: %d kB", &rssKB)
					snap.QEMURSSMiB = rssKB / 1024
				}
				if strings.HasPrefix(line, "State:") {
					snap.QEMUStatus = strings.TrimSpace(strings.TrimPrefix(line, "State:"))
				}
			}
		} else {
			snap.QEMUStatus = "exited"
		}
	}

	return snap
}
