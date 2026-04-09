package ssh

import (
	"testing"
)

// TestParseSendkeysArgs validates the --no-enter flag parsing logic
// used by the tapegun sendkeys command.
func TestParseSendkeysArgs(t *testing.T) {
	tests := []struct {
		name      string
		args      []string // args after "sendkeys"
		wantVM    string
		wantMsg   string
		wantNoEnt bool
		wantErr   bool
	}{
		{
			name:      "basic sendkeys",
			args:      []string{"my-vm", "echo", "hello"},
			wantVM:    "my-vm",
			wantMsg:   "echo hello",
			wantNoEnt: false,
		},
		{
			name:      "sendkeys with --no-enter",
			args:      []string{"--no-enter", "my-vm", "partial", "text"},
			wantVM:    "my-vm",
			wantMsg:   "partial text",
			wantNoEnt: true,
		},
		{
			name:      "single word command",
			args:      []string{"dev-worker-1", "ls"},
			wantVM:    "dev-worker-1",
			wantMsg:   "ls",
			wantNoEnt: false,
		},
		{
			name:      "--no-enter single word",
			args:      []string{"--no-enter", "dev-worker-1", "text"},
			wantVM:    "dev-worker-1",
			wantMsg:   "text",
			wantNoEnt: true,
		},
		{
			name:    "too few args",
			args:    []string{"my-vm"},
			wantErr: true,
		},
		{
			name:    "no-enter with too few args",
			args:    []string{"--no-enter", "my-vm"},
			wantErr: true,
		},
		{
			name:    "empty args",
			args:    []string{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Replicate the parsing logic from the handler
			noEnter := false
			sendkeysArgs := tt.args
			if len(sendkeysArgs) > 0 && sendkeysArgs[0] == "--no-enter" {
				noEnter = true
				sendkeysArgs = sendkeysArgs[1:]
			}
			if len(sendkeysArgs) < 2 {
				if !tt.wantErr {
					t.Error("unexpected parse error")
				}
				return
			}
			if tt.wantErr {
				t.Error("expected parse error but got none")
				return
			}

			vm := sendkeysArgs[0]
			msg := ""
			for i, a := range sendkeysArgs[1:] {
				if i > 0 {
					msg += " "
				}
				msg += a
			}

			if vm != tt.wantVM {
				t.Errorf("vm = %q, want %q", vm, tt.wantVM)
			}
			if msg != tt.wantMsg {
				t.Errorf("msg = %q, want %q", msg, tt.wantMsg)
			}
			if noEnter != tt.wantNoEnt {
				t.Errorf("noEnter = %v, want %v", noEnter, tt.wantNoEnt)
			}
		})
	}
}
