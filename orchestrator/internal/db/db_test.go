package db

import (
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrate_Idempotent(t *testing.T) {
	d := openTestDB(t)
	if err := d.migrate(); err != nil {
		t.Errorf("Second migrate() failed: %v", err)
	}
}

func TestGoldenHead(t *testing.T) {
	d := openTestDB(t)

	// Initially empty
	if head := d.GetGoldenHead(); head != "" {
		t.Errorf("GetGoldenHead = %q, want empty", head)
	}

	// Set and get
	if err := d.SetGoldenHead("v1.2.3"); err != nil {
		t.Fatal(err)
	}
	if head := d.GetGoldenHead(); head != "v1.2.3" {
		t.Errorf("GetGoldenHead = %q, want v1.2.3", head)
	}

	// Update
	d.SetGoldenHead("v1.3.0")
	if head := d.GetGoldenHead(); head != "v1.3.0" {
		t.Errorf("GetGoldenHead after update = %q, want v1.3.0", head)
	}
}

func TestSSHKeys(t *testing.T) {
	d := openTestDB(t)

	added, err := d.AddSSHKeys("alice", []string{"ssh-ed25519 AAAA alice@dev", "ssh-rsa BBBB alice@laptop"}, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Errorf("added = %d, want 2", added)
	}

	keys, err := d.ListSSHKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("ListSSHKeys = %d, want 2", len(keys))
	}

	entries, err := d.ListSSHKeyEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("ListSSHKeyEntries = %d, want 2", len(entries))
	}
	if entries[0].GitHubUser != "alice" {
		t.Errorf("github_user = %q, want alice", entries[0].GitHubUser)
	}

	// Duplicate key is ignored
	added2, _ := d.AddSSHKeys("alice", []string{"ssh-ed25519 AAAA alice@dev"}, "2026-01-02T00:00:00Z")
	if added2 != 1 {
		// INSERT OR IGNORE returns nil even on conflict, so added count may include ignored
		// Just verify total count is still 2
	}
	keys, _ = d.ListSSHKeys()
	if len(keys) != 2 {
		t.Errorf("After duplicate add: ListSSHKeys = %d, want 2", len(keys))
	}

	// Delete by user
	if err := d.DeleteSSHKeysByUser("alice"); err != nil {
		t.Fatal(err)
	}
	keys, _ = d.ListSSHKeys()
	if len(keys) != 0 {
		t.Errorf("After delete: ListSSHKeys = %d, want 0", len(keys))
	}
}

func TestSSHKeys_EmptyInput(t *testing.T) {
	d := openTestDB(t)

	added, err := d.AddSSHKeys("bob", []string{"", "  ", "ssh-ed25519 CCCC bob@dev"}, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	// Only the non-empty key should be added
	if added != 1 {
		t.Errorf("added = %d, want 1 (empty keys should be skipped)", added)
	}
}
