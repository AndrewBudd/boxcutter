package db

// KnowledgeEntry is a row from the knowledge table.
type KnowledgeEntry struct {
	Agent     string `json:"agent"`
	Key       string `json:"key"`
	Value     string `json:"value"`
	UpdatedAt string `json:"updated_at"`
}

// CreateKnowledgeTable creates the knowledge table if it does not already exist.
// Called from InitSchema / main.go after opening the database.
func (db *DB) CreateKnowledgeTable() error {
	_, err := db.conn.Exec(`
CREATE TABLE IF NOT EXISTS knowledge (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	agent      TEXT NOT NULL,
	key        TEXT NOT NULL,
	value      TEXT NOT NULL,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(agent, key)
)`)
	return err
}

// PutKnowledge upserts a knowledge entry for (agent, key).
func (db *DB) PutKnowledge(agent, key, value string) error {
	_, err := db.conn.Exec(`
INSERT INTO knowledge (agent, key, value)
VALUES (?, ?, ?)
ON CONFLICT(agent, key) DO UPDATE SET
	value      = excluded.value,
	updated_at = CURRENT_TIMESTAMP`,
		agent, key, value)
	return err
}

// GetKnowledge retrieves the value for (agent, key).
func (db *DB) GetKnowledge(agent, key string) (string, error) {
	var value string
	err := db.conn.QueryRow(
		`SELECT value FROM knowledge WHERE agent = ? AND key = ?`,
		agent, key,
	).Scan(&value)
	return value, err
}

// ListKnowledge returns all knowledge entries for the given agent.
func (db *DB) ListKnowledge(agent string) ([]KnowledgeEntry, error) {
	rows, err := db.conn.Query(
		`SELECT agent, key, value, updated_at FROM knowledge WHERE agent = ? ORDER BY key`,
		agent,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []KnowledgeEntry
	for rows.Next() {
		var e KnowledgeEntry
		if err := rows.Scan(&e.Agent, &e.Key, &e.Value, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// ListAllKnowledge returns all knowledge entries across all agents.
func (db *DB) ListAllKnowledge() ([]KnowledgeEntry, error) {
	rows, err := db.conn.Query(
		`SELECT agent, key, value, updated_at FROM knowledge ORDER BY agent, key`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []KnowledgeEntry
	for rows.Next() {
		var e KnowledgeEntry
		if err := rows.Scan(&e.Agent, &e.Key, &e.Value, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// DeleteKnowledge removes the knowledge entry for (agent, key).
func (db *DB) DeleteKnowledge(agent, key string) error {
	_, err := db.conn.Exec(
		`DELETE FROM knowledge WHERE agent = ? AND key = ?`,
		agent, key,
	)
	return err
}
