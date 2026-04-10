package ssh

import (
	"fmt"
	"testing"
)

func TestIsCapacityError(t *testing.T) {
	tests := []struct {
		err  string
		want bool
	}{
		{"all nodes failed", true},
		{"no active nodes", true},
		{"no reachable nodes", true},
		{"VM 'foo' already exists", false},
		{"connection refused", false},
		{"invalid JSON: unexpected EOF", false},
	}
	for _, tt := range tests {
		got := isCapacityError(fmt.Errorf("%s", tt.err))
		if got != tt.want {
			t.Errorf("isCapacityError(%q) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
