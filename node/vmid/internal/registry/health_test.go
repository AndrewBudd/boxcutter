package registry

import (
	"testing"
	"time"
)

func TestSetAndGetHealth(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	report := &HealthReport{
		Timestamp: time.Now(),
		Health:    "healthy",
		HealthChecks: map[string]bool{
			"binary_exec": true,
			"network":     true,
			"metadata":    true,
			"disk":        true,
			"services":    true,
		},
	}
	if !r.SetHealth("vm-1", report) {
		t.Fatal("SetHealth returned false")
	}

	got, ok := r.GetHealth("vm-1")
	if !ok {
		t.Fatal("GetHealth returned false")
	}
	if got.Health != "healthy" {
		t.Fatalf("Health = %q, want %q", got.Health, "healthy")
	}
	if !got.HealthChecks["binary_exec"] {
		t.Fatal("binary_exec should be true")
	}
	if !got.HealthChecks["network"] {
		t.Fatal("network should be true")
	}
}

func TestSetHealthUnknownVM(t *testing.T) {
	r := New()
	report := &HealthReport{Health: "healthy"}
	if r.SetHealth("nonexistent", report) {
		t.Fatal("SetHealth should return false for unknown VM")
	}
}

func TestGetHealthUnknownVM(t *testing.T) {
	r := New()
	_, ok := r.GetHealth("nonexistent")
	if ok {
		t.Fatal("GetHealth should return false for unknown VM")
	}
}

func TestGetHealthNilReport(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	got, ok := r.GetHealth("vm-1")
	if !ok {
		t.Fatal("GetHealth should return true for known VM")
	}
	if got != nil {
		t.Fatal("GetHealth should return nil for VM without health report")
	}
}

func TestHealthReportIsHealthy(t *testing.T) {
	tests := []struct {
		name   string
		report *HealthReport
		want   bool
	}{
		{"nil report", nil, false},
		{"healthy", &HealthReport{Health: "healthy"}, true},
		{"degraded", &HealthReport{Health: "degraded"}, false},
		{"unhealthy", &HealthReport{Health: "unhealthy"}, false},
		{"empty", &HealthReport{Health: ""}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.report.IsHealthy()
			if got != tt.want {
				t.Fatalf("IsHealthy() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHealthReportOverwrite(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	r.SetHealth("vm-1", &HealthReport{Health: "unhealthy"})
	r.SetHealth("vm-1", &HealthReport{Health: "healthy"})

	got, _ := r.GetHealth("vm-1")
	if got.Health != "healthy" {
		t.Fatalf("Health = %q, want %q after overwrite", got.Health, "healthy")
	}
}

func TestHealthReportWithDetails(t *testing.T) {
	r := New()
	r.Register(&VMRecord{VMID: "vm-1", IP: "10.0.0.2", Mark: 100})

	report := &HealthReport{
		Timestamp: time.Now(),
		Health:    "degraded",
		HealthChecks: map[string]bool{
			"binary_exec": true,
			"network":     false,
			"metadata":    false,
			"disk":        true,
			"services":    true,
		},
		Details: map[string]string{
			"network": "gateway unreachable",
		},
	}
	r.SetHealth("vm-1", report)

	got, _ := r.GetHealth("vm-1")
	if got.Health != "degraded" {
		t.Fatalf("Health = %q, want %q", got.Health, "degraded")
	}
	if got.Details["network"] != "gateway unreachable" {
		t.Fatalf("Details[network] = %q, want %q", got.Details["network"], "gateway unreachable")
	}
	if got.HealthChecks["network"] {
		t.Fatal("network check should be false")
	}
}
