package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Node is used by the scheduler package for node selection.
// Runtime node state is managed by the state package, not SQLite.
type Node struct {
	ID            string `json:"id"`
	TailscaleName string `json:"tailscale_name"`
	TailscaleIP   string `json:"tailscale_ip"`
	BridgeIP      string `json:"bridge_ip"`
	APIAddr       string `json:"api_addr"`
	Status        string `json:"status"`
	RegisteredAt  string `json:"registered_at"`
	LastHeartbeat string `json:"last_heartbeat"`

	// Transient fields — populated at runtime from node health, not stored
	RAMTotalMIB     int `json:"ram_total_mib,omitempty"`
	RAMAllocatedMIB int `json:"ram_allocated_mib,omitempty"`
	RAMAvailableMIB int `json:"ram_available_mib,omitempty"`
	DiskTotalMB     int `json:"disk_total_mb,omitempty"`
	DiskUsedMB      int `json:"disk_used_mb,omitempty"`
	DiskVMsMB       int `json:"disk_vms_mb,omitempty"`
	VMsRunning      int `json:"vms_running,omitempty"`
}

type DB struct {
	conn *sql.DB
}

func Open(path string) (*DB, error) {
	os.MkdirAll(filepath.Dir(path), 0755)

	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	conn.Exec("PRAGMA journal_mode=WAL")
	conn.Exec("PRAGMA foreign_keys=ON")

	db := &DB{conn: conn}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrating database: %w", err)
	}

	return db, nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

// Conn returns the underlying *sql.DB for use by other packages.
func (db *DB) Conn() *sql.DB {
	return db.conn
}

func (db *DB) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS golden_config (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS ssh_keys (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		github_user TEXT NOT NULL,
		public_key TEXT NOT NULL UNIQUE,
		added_at TEXT NOT NULL
	);
	`

	_, err := db.conn.Exec(schema)
	return err
}

// --- Golden config operations ---

// GetGoldenHead returns the current golden head version.
func (db *DB) GetGoldenHead() string {
	var val string
	db.conn.QueryRow(`SELECT value FROM golden_config WHERE key='head_version'`).Scan(&val)
	return val
}

// SetGoldenHead sets the golden head version.
func (db *DB) SetGoldenHead(version string) error {
	_, err := db.conn.Exec(`INSERT INTO golden_config (key, value) VALUES ('head_version', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, version)
	return err
}

// --- SSH key operations ---

type SSHKey struct {
	ID         int    `json:"id"`
	GitHubUser string `json:"github_user"`
	PublicKey  string `json:"public_key"`
	AddedAt    string `json:"added_at"`
}

func (db *DB) AddSSHKeys(githubUser string, keys []string, addedAt string) (int, error) {
	added := 0
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		_, err := db.conn.Exec(
			`INSERT OR IGNORE INTO ssh_keys (github_user, public_key, added_at) VALUES (?, ?, ?)`,
			githubUser, k, addedAt)
		if err == nil {
			added++
		}
	}
	return added, nil
}

func (db *DB) ListSSHKeys() ([]string, error) {
	rows, err := db.conn.Query(`SELECT public_key FROM ssh_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (db *DB) ListSSHKeyEntries() ([]*SSHKey, error) {
	rows, err := db.conn.Query(`SELECT id, github_user, public_key, added_at FROM ssh_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []*SSHKey
	for rows.Next() {
		var k SSHKey
		if rows.Scan(&k.ID, &k.GitHubUser, &k.PublicKey, &k.AddedAt) == nil {
			keys = append(keys, &k)
		}
	}
	return keys, nil
}

func (db *DB) DeleteSSHKeysByUser(githubUser string) error {
	_, err := db.conn.Exec(`DELETE FROM ssh_keys WHERE github_user=?`, githubUser)
	return err
}
