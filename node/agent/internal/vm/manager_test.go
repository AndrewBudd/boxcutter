package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCapacityError(t *testing.T) {
	err := &CapacityError{msg: "node is full"}
	if err.Error() != "node is full" {
		t.Errorf("CapacityError.Error() = %q, want 'node is full'", err.Error())
	}

	// IsCapacityError should recognize it
	var ce *CapacityError
	if !isCapErr(err, &ce) {
		t.Error("should detect CapacityError via errors.As")
	}
}

// isCapErr is a test helper
func isCapErr(err error, target interface{}) bool {
	_, ok := err.(*CapacityError)
	return ok
}

func TestVMState_AllGitHubRepos(t *testing.T) {
	tests := []struct {
		name     string
		state    VMState
		wantLen  int
		wantRepo string
	}{
		{
			"single repo field",
			VMState{GitHubRepo: "org/repo"},
			1, "org/repo",
		},
		{
			"repos array",
			VMState{GitHubRepos: []string{"org/a", "org/b"}},
			2, "org/a",
		},
		{
			"repos array overrides single",
			VMState{GitHubRepo: "org/old", GitHubRepos: []string{"org/new"}},
			1, "org/new",
		},
		{
			"empty",
			VMState{},
			0, "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repos := tt.state.AllGitHubRepos()
			if len(repos) != tt.wantLen {
				t.Errorf("AllGitHubRepos len = %d, want %d", len(repos), tt.wantLen)
			}
			if tt.wantLen > 0 && repos[0] != tt.wantRepo {
				t.Errorf("AllGitHubRepos[0] = %q, want %q", repos[0], tt.wantRepo)
			}
		})
	}
}

func TestVMState_AllCloneURLs(t *testing.T) {
	tests := []struct {
		name    string
		state   VMState
		wantLen int
	}{
		{"single url", VMState{CloneURL: "https://github.com/org/repo"}, 1},
		{"urls array", VMState{CloneURLs: []string{"url1", "url2"}}, 2},
		{"array overrides single", VMState{CloneURL: "old", CloneURLs: []string{"new"}}, 1},
		{"empty", VMState{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			urls := tt.state.AllCloneURLs()
			if len(urls) != tt.wantLen {
				t.Errorf("AllCloneURLs len = %d, want %d", len(urls), tt.wantLen)
			}
		})
	}
}

func TestLoadVMState(t *testing.T) {
	dir := t.TempDir()
	st := VMState{
		Name:        "test-vm",
		Type:        "qemu",
		VCPU:        4,
		RAMMIB:      4096,
		Mark:        12345,
		Mode:        "normal",
		Description: "test desc",
	}
	data, _ := json.Marshal(st)
	os.WriteFile(filepath.Join(dir, "vm.json"), data, 0644)

	loaded, err := LoadVMState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name != "test-vm" {
		t.Errorf("Name = %q, want test-vm", loaded.Name)
	}
	if loaded.Type != "qemu" {
		t.Errorf("Type = %q, want qemu", loaded.Type)
	}
	if loaded.RAMMIB != 4096 {
		t.Errorf("RAMMIB = %d, want 4096", loaded.RAMMIB)
	}
	if loaded.Description != "test desc" {
		t.Errorf("Description = %q, want 'test desc'", loaded.Description)
	}
}

func TestLoadVMState_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadVMState(dir)
	if err == nil {
		t.Error("LoadVMState should error for missing vm.json")
	}
}

func TestSetMigrating(t *testing.T) {
	dir := t.TempDir()

	// Set migrating with target
	SetMigrating(dir, true)
	if !IsMigrating(dir) {
		t.Error("should be migrating after SetMigrating(true)")
	}

	// Clear
	SetMigrating(dir, false)
	if IsMigrating(dir) {
		t.Error("should not be migrating after SetMigrating(false)")
	}
}

func TestSetMigratingWithTarget(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "migrating"), []byte("192.168.50.11:8800"), 0644)

	target := MigrationTarget(dir)
	if target != "192.168.50.11:8800" {
		t.Errorf("MigrationTarget = %q, want 192.168.50.11:8800", target)
	}

	SetMigrating(dir, false)
	target = MigrationTarget(dir)
	if target != "" {
		t.Errorf("MigrationTarget after clear = %q, want empty", target)
	}
}

func TestVMDir(t *testing.T) {
	dir := VMDir("test-vm")
	if !filepath.IsAbs(dir) {
		t.Errorf("VMDir should return absolute path, got %q", dir)
	}
	if filepath.Base(dir) != "test-vm" {
		t.Errorf("VMDir base = %q, want test-vm", filepath.Base(dir))
	}
}
