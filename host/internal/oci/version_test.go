package oci

import "testing"

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input                  string
		major, minor, patch    int
		wantErr                bool
	}{
		{"v0.37.4", 0, 37, 4, false},
		{"v1.2.3", 1, 2, 3, false},
		{"0.37.4", 0, 37, 4, false},
		{"v0.37.4-rc1", 0, 37, 4, false},
		{"latest", 0, 0, 0, true},
		{"abc123", 0, 0, 0, true},
		{"", 0, 0, 0, true},
	}

	for _, tt := range tests {
		maj, min, pat, err := parseSemver(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseSemver(%q): expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSemver(%q): unexpected error: %v", tt.input, err)
			continue
		}
		if maj != tt.major || min != tt.minor || pat != tt.patch {
			t.Errorf("parseSemver(%q) = %d.%d.%d, want %d.%d.%d",
				tt.input, maj, min, pat, tt.major, tt.minor, tt.patch)
		}
	}
}

func TestVersionRegression(t *testing.T) {
	// parseSemver comparison logic (unit test without registry access)
	cases := []struct {
		current, new string
		regression   bool
	}{
		{"v0.37.4", "v0.37.5", false},
		{"v0.37.4", "v0.38.0", false},
		{"v0.37.4", "v0.37.4", false},
		{"v0.37.4", "v0.37.3", true},
		{"v0.37.4", "v0.36.0", true},
		{"v1.0.0", "v0.99.99", true},
	}

	for _, tt := range cases {
		curMaj, curMin, curPat, _ := parseSemver(tt.current)
		newMaj, newMin, newPat, _ := parseSemver(tt.new)
		curTotal := curMaj*1000000 + curMin*1000 + curPat
		newTotal := newMaj*1000000 + newMin*1000 + newPat
		gotRegression := newTotal < curTotal
		if gotRegression != tt.regression {
			t.Errorf("%s -> %s: got regression=%v, want %v", tt.current, tt.new, gotRegression, tt.regression)
		}
	}
}
