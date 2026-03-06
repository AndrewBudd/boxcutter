package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type DB struct {
	conn *sql.DB
}

func Open(path string) (*DB, error) {
	os.MkdirAll(filepath.Dir(path), 0755)

	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// WAL mode for concurrent reads
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

func (db *DB) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS nodes (
		id TEXT PRIMARY KEY,
		tailscale_name TEXT NOT NULL,
		tailscale_ip TEXT,
		bridge_ip TEXT,
		api_addr TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'active',
		registered_at TEXT NOT NULL,
		last_heartbeat TEXT
	);

	CREATE TABLE IF NOT EXISTS vms (
		name TEXT PRIMARY KEY,
		node_id TEXT NOT NULL REFERENCES nodes(id),
		status TEXT DEFAULT 'running'
	);

	CREATE TABLE IF NOT EXISTS ssh_keys (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		github_user TEXT NOT NULL,
		public_key TEXT NOT NULL UNIQUE,
		added_at TEXT NOT NULL
	);
	`

	_, err := db.conn.Exec(schema)
	if err != nil {
		return err
	}

	// Migrate: add bridge_ip column if it doesn't exist (for existing databases)
	db.conn.Exec(`ALTER TABLE nodes ADD COLUMN bridge_ip TEXT`)

	// Migrate: drop old columns from vms if they exist (sqlite doesn't support DROP COLUMN before 3.35)
	// Instead, just ignore old columns — the new queries only select name, node_id, status
	return nil
}

// --- Node operations ---

type Node struct {
	ID            string `json:"id"`
	TailscaleName string `json:"tailscale_name"`
	TailscaleIP   string `json:"tailscale_ip"`
	BridgeIP      string `json:"bridge_ip"`
	APIAddr       string `json:"api_addr"`
	Status        string `json:"status"`
	RegisteredAt  string `json:"registered_at"`
	LastHeartbeat string `json:"last_heartbeat"`

	// Transient fields — populated at runtime from node health, not stored in DB
	RAMTotalMIB     int `json:"ram_total_mib,omitempty"`
	RAMAllocatedMIB int `json:"ram_allocated_mib,omitempty"`
	VMsRunning      int `json:"vms_running,omitempty"`
}

func (db *DB) RegisterNode(n *Node) error {
	_, err := db.conn.Exec(`
		INSERT INTO nodes (id, tailscale_name, tailscale_ip, bridge_ip, api_addr, status, registered_at, last_heartbeat)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			tailscale_ip=excluded.tailscale_ip,
			bridge_ip=excluded.bridge_ip,
			api_addr=excluded.api_addr,
			status=excluded.status,
			last_heartbeat=excluded.last_heartbeat`,
		n.ID, n.TailscaleName, n.TailscaleIP, n.BridgeIP, n.APIAddr, n.Status,
		n.RegisteredAt, n.LastHeartbeat,
	)
	return err
}

func (db *DB) UpdateNodeHeartbeat(id, lastHeartbeat string) error {
	_, err := db.conn.Exec(`UPDATE nodes SET last_heartbeat=? WHERE id=?`, lastHeartbeat, id)
	return err
}

func (db *DB) SetNodeStatus(id, status string) error {
	_, err := db.conn.Exec(`UPDATE nodes SET status=? WHERE id=?`, status, id)
	return err
}

func (db *DB) GetNode(id string) (*Node, error) {
	row := db.conn.QueryRow(`SELECT id, tailscale_name, COALESCE(tailscale_ip,''), COALESCE(bridge_ip,''), api_addr, status, registered_at, COALESCE(last_heartbeat,'') FROM nodes WHERE id=?`, id)
	var n Node
	err := row.Scan(&n.ID, &n.TailscaleName, &n.TailscaleIP, &n.BridgeIP, &n.APIAddr, &n.Status,
		&n.RegisteredAt, &n.LastHeartbeat)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (db *DB) ListNodes() ([]*Node, error) {
	rows, err := db.conn.Query(`SELECT id, tailscale_name, COALESCE(tailscale_ip,''), COALESCE(bridge_ip,''), api_addr, status, registered_at, COALESCE(last_heartbeat,'') FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []*Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.TailscaleName, &n.TailscaleIP, &n.BridgeIP, &n.APIAddr, &n.Status,
			&n.RegisteredAt, &n.LastHeartbeat); err != nil {
			continue
		}
		nodes = append(nodes, &n)
	}
	return nodes, nil
}

func (db *DB) DeleteNode(id string) error {
	_, err := db.conn.Exec(`DELETE FROM nodes WHERE id=?`, id)
	return err
}

// ActiveNodes returns nodes with status "active".
func (db *DB) ActiveNodes() ([]*Node, error) {
	rows, err := db.conn.Query(`SELECT id, tailscale_name, COALESCE(tailscale_ip,''), COALESCE(bridge_ip,''), api_addr, status, registered_at, COALESCE(last_heartbeat,'') FROM nodes WHERE status='active' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []*Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.TailscaleName, &n.TailscaleIP, &n.BridgeIP, &n.APIAddr, &n.Status,
			&n.RegisteredAt, &n.LastHeartbeat); err != nil {
			continue
		}
		nodes = append(nodes, &n)
	}
	return nodes, nil
}

// --- VM operations ---
// The orchestrator only tracks (name, node_id, status).
// All detail (vcpu, ram, disk, tailscale_ip, etc.) lives on the node.

type VM struct {
	Name   string `json:"name"`
	NodeID string `json:"node_id"`
	Status string `json:"status"`
}

func (db *DB) CreateVM(v *VM) error {
	_, err := db.conn.Exec(`INSERT INTO vms (name, node_id, status) VALUES (?, ?, ?)`,
		v.Name, v.NodeID, v.Status)
	return err
}

func (db *DB) UpdateVMStatus(name, status string) error {
	_, err := db.conn.Exec(`UPDATE vms SET status=? WHERE name=?`, status, name)
	return err
}

func (db *DB) UpdateVMNode(name, nodeID string) error {
	_, err := db.conn.Exec(`UPDATE vms SET node_id=? WHERE name=?`, nodeID, name)
	return err
}

func (db *DB) GetVM(name string) (*VM, error) {
	row := db.conn.QueryRow(`SELECT name, node_id, status FROM vms WHERE name=?`, name)
	var v VM
	err := row.Scan(&v.Name, &v.NodeID, &v.Status)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (db *DB) ListVMs() ([]*VM, error) {
	rows, err := db.conn.Query(`SELECT name, node_id, status FROM vms ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vms []*VM
	for rows.Next() {
		var v VM
		if err := rows.Scan(&v.Name, &v.NodeID, &v.Status); err != nil {
			continue
		}
		vms = append(vms, &v)
	}
	return vms, nil
}

func (db *DB) ListVMsByNode(nodeID string) ([]*VM, error) {
	rows, err := db.conn.Query(`SELECT name, node_id, status FROM vms WHERE node_id=? ORDER BY name`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vms []*VM
	for rows.Next() {
		var v VM
		if err := rows.Scan(&v.Name, &v.NodeID, &v.Status); err != nil {
			continue
		}
		vms = append(vms, &v)
	}
	return vms, nil
}

func (db *DB) DeleteVM(name string) error {
	_, err := db.conn.Exec(`DELETE FROM vms WHERE name=?`, name)
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
