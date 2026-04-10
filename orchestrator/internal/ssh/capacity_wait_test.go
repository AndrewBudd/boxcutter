package ssh

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestPostStreamSurfacesHTTPErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantErr    string
	}{
		{
			name:       "no active nodes (pre-stream 503)",
			statusCode: http.StatusServiceUnavailable,
			body:       "no active nodes\n",
			wantErr:    "no active nodes",
		},
		{
			name:       "no reachable nodes (pre-stream 503)",
			statusCode: http.StatusServiceUnavailable,
			body:       "no reachable nodes\n",
			wantErr:    "no reachable nodes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tt.body, tt.statusCode)
			}))
			defer srv.Close()

			h := &Handler{apiBase: srv.URL}
			_, err := h.postStream("/api/vms", nil, nil)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if err.Error() != tt.wantErr {
				t.Errorf("postStream error = %q, want %q", err.Error(), tt.wantErr)
			}
			if !isCapacityError(err) {
				t.Errorf("isCapacityError(%q) = false, want true", err.Error())
			}
		})
	}
}
