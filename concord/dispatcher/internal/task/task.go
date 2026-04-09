package task

import "time"

// Task represents a unit of work to be executed on a worker VM.
type Task struct {
	ID    string `json:"id"`
	Steps []Step `json:"steps"`
}

// Step is a single command within a task.
type Step struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Workdir string `json:"workdir,omitempty"`
}

// Status represents the current state of a task on a worker VM.
type Status struct {
	Status    string    `json:"status"` // ready, running, completed, failed, error
	Step      string    `json:"step"`
	ExitCode  int       `json:"exit_code"`
	Detail    string    `json:"detail"`
	UpdatedAt time.Time `json:"updated_at"`
}

// IsTerminal returns true if the task has finished (success or failure).
func (s *Status) IsTerminal() bool {
	switch s.Status {
	case "completed", "failed", "error":
		return true
	}
	return false
}
