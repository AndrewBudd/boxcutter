package vm

import (
	"testing"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
		wantErr  bool
	}{
		{"50G", 50 * 1024 * 1024 * 1024, false},
		{"1G", 1024 * 1024 * 1024, false},
		{"512M", 512 * 1024 * 1024, false},
		{"1T", 1024 * 1024 * 1024 * 1024, false},
		{"2048M", 2048 * 1024 * 1024, false},
		{"", 0, true},
		{"invalid", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseSize(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseSize(%q) should error", tt.input)
				}
				return
			}
			if err != nil {
				t.Errorf("parseSize(%q) error: %v", tt.input, err)
				return
			}
			if got != tt.expected {
				t.Errorf("parseSize(%q) = %d, want %d", tt.input, got, tt.expected)
			}
		})
	}
}

func TestGoldenQCOW2Path(t *testing.T) {
	tests := []struct {
		ext4Path string
		expected string
	}{
		{"/var/lib/boxcutter/golden/abc123.ext4", "/var/lib/boxcutter/golden/abc123.qcow2"},
		{"/var/lib/boxcutter/golden/rootfs.ext4", "/var/lib/boxcutter/golden/rootfs.qcow2"},
	}

	for _, tt := range tests {
		got := GoldenQCOW2Path(tt.ext4Path)
		if got != tt.expected {
			t.Errorf("GoldenQCOW2Path(%q) = %q, want %q", tt.ext4Path, got, tt.expected)
		}
	}
}
