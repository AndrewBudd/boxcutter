package worker

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AndrewBudd/boxcutter/concord/dispatcher/internal/task"
)

func TestExec(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/api/vms/test-vm/exec") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		if req["command"] != "echo hello" {
			t.Errorf("unexpected command: %s", req["command"])
		}
		json.NewEncoder(w).Encode(ExecResult{Output: "hello\n", ExitCode: 0})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	result, err := c.Exec("test-vm", "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "hello\n" {
		t.Errorf("output = %q, want %q", result.Output, "hello\n")
	}
	if result.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", result.ExitCode)
	}
}

func TestCopyTo(t *testing.T) {
	var gotBody string
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		gotPath = r.URL.Query().Get("path")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := NewClient(server.URL)
	err := c.CopyTo("test-vm", "/tmp/test.txt", strings.NewReader("file content"))
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/tmp/test.txt" {
		t.Errorf("path = %q, want %q", gotPath, "/tmp/test.txt")
	}
	if gotBody != "file content" {
		t.Errorf("body = %q, want %q", gotBody, "file content")
	}
}

func TestCopyFrom(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		path := r.URL.Query().Get("path")
		if path != "/opt/concord-agent/outbox/result.tar.gz" {
			t.Errorf("unexpected path: %s", path)
		}
		w.Write([]byte("binary-data"))
	}))
	defer server.Close()

	c := NewClient(server.URL)
	var buf strings.Builder
	err := c.CopyFrom("test-vm", "/opt/concord-agent/outbox/result.tar.gz", &buf)
	if err != nil {
		t.Fatal(err)
	}
	if buf.String() != "binary-data" {
		t.Errorf("output = %q, want %q", buf.String(), "binary-data")
	}
}

func TestPollStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ExecResult{
			Output:   `{"status":"completed","step":"","exit_code":0,"detail":"all 3 steps completed","updated_at":"2026-04-08T10:00:00Z"}`,
			ExitCode: 0,
		})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	status, err := c.PollStatus("test-vm")
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "completed" {
		t.Errorf("status = %q, want %q", status.Status, "completed")
	}
	if status.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", status.ExitCode)
	}
}

func TestSubmitTask(t *testing.T) {
	var cpToBody []byte
	var execCmd string
	callCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if strings.Contains(r.URL.Path, "/cp-to") {
			body, _ := io.ReadAll(r.Body)
			cpToBody = body
			w.WriteHeader(http.StatusNoContent)
		} else if strings.Contains(r.URL.Path, "/exec") {
			var req map[string]string
			json.NewDecoder(r.Body).Decode(&req)
			execCmd = req["command"]
			json.NewEncoder(w).Encode(ExecResult{Output: "", ExitCode: 0})
		}
	}))
	defer server.Close()

	c := NewClient(server.URL)
	tsk := &task.Task{
		ID: "task-1",
		Steps: []task.Step{
			{Name: "build", Command: "make build"},
			{Name: "test", Command: "make test"},
		},
	}

	err := c.SubmitTask("test-vm", tsk)
	if err != nil {
		t.Fatal(err)
	}

	// Verify task payload was sent via cp-to
	var sentTask task.Task
	if err := json.Unmarshal(cpToBody, &sentTask); err != nil {
		t.Fatalf("failed to parse sent task: %v", err)
	}
	if sentTask.ID != "task-1" {
		t.Errorf("task ID = %q, want %q", sentTask.ID, "task-1")
	}
	if len(sentTask.Steps) != 2 {
		t.Errorf("steps = %d, want 2", len(sentTask.Steps))
	}

	// Verify run-task.sh was called via exec
	if execCmd != "/opt/concord-agent/run-task.sh" {
		t.Errorf("exec command = %q, want %q", execCmd, "/opt/concord-agent/run-task.sh")
	}
}

func TestDestroyVM(t *testing.T) {
	var gotMethod string
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := NewClient(server.URL)
	err := c.DestroyVM("test-vm")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "DELETE" {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/vms/test-vm" {
		t.Errorf("path = %q, want %q", gotPath, "/api/vms/test-vm")
	}
}

func TestWaitForCompletion(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var status string
		if callCount < 3 {
			status = `{"status":"running","step":"build","exit_code":0,"detail":"step 1/2","updated_at":"2026-04-08T10:00:00Z"}`
		} else {
			status = `{"status":"completed","step":"","exit_code":0,"detail":"all done","updated_at":"2026-04-08T10:00:05Z"}`
		}
		json.NewEncoder(w).Encode(ExecResult{Output: status, ExitCode: 0})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	result, err := c.WaitForCompletion("test-vm", 10*time.Millisecond, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Errorf("status = %q, want %q", result.Status, "completed")
	}
	if callCount < 3 {
		t.Errorf("expected at least 3 polls, got %d", callCount)
	}
}

func TestWaitForCompletionTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ExecResult{
			Output:   `{"status":"running","step":"build","exit_code":0,"detail":"still going","updated_at":"2026-04-08T10:00:00Z"}`,
			ExitCode: 0,
		})
	}))
	defer server.Close()

	c := NewClient(server.URL)
	_, err := c.WaitForCompletion("test-vm", 10*time.Millisecond, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want timeout message", err.Error())
	}
}

func TestTaskStatusIsTerminal(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{"ready", false},
		{"running", false},
		{"completed", true},
		{"failed", true},
		{"error", true},
	}
	for _, tt := range tests {
		s := &task.Status{Status: tt.status}
		if got := s.IsTerminal(); got != tt.want {
			t.Errorf("Status(%q).IsTerminal() = %v, want %v", tt.status, got, tt.want)
		}
	}
}
