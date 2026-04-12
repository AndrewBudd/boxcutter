package db

import (
	"fmt"
	"time"
)

// Messaging schema for SNS/SQS-style topics and queues (#190).

const messagingSchema = `
CREATE TABLE IF NOT EXISTS topics (
	name TEXT PRIMARY KEY,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS queues (
	name TEXT PRIMARY KEY,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS subscriptions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	topic_name TEXT NOT NULL REFERENCES topics(name),
	queue_name TEXT NOT NULL REFERENCES queues(name),
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(topic_name, queue_name)
);

CREATE TABLE IF NOT EXISTS messages (
	id TEXT PRIMARY KEY,
	topic_name TEXT,
	queue_name TEXT NOT NULL REFERENCES queues(name),
	body TEXT NOT NULL,
	from_agent TEXT,
	priority TEXT DEFAULT 'normal',
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	read_at DATETIME,
	acked_at DATETIME
);

CREATE INDEX IF NOT EXISTS idx_messages_queue ON messages(queue_name, acked_at);
CREATE INDEX IF NOT EXISTS idx_messages_created ON messages(created_at);
`

type Topic struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Queue struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Depth     int       `json:"depth,omitempty"` // unacked message count
}

type Subscription struct {
	ID        int64     `json:"id"`
	TopicName string    `json:"topic_name"`
	QueueName string    `json:"queue_name"`
	CreatedAt time.Time `json:"created_at"`
}

type Message struct {
	ID        string     `json:"id"`
	TopicName string     `json:"topic_name,omitempty"`
	QueueName string     `json:"queue_name"`
	Body      string     `json:"body"`
	FromAgent string     `json:"from_agent,omitempty"`
	Priority  string     `json:"priority"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
	AckedAt   *time.Time `json:"acked_at,omitempty"`
}

// CreateMessagingTables initializes the messaging schema.
func (db *DB) CreateMessagingTables() error {
	_, err := db.conn.Exec(messagingSchema)
	return err
}

// --- Topics ---

func (db *DB) CreateTopic(name string) error {
	_, err := db.conn.Exec("INSERT OR IGNORE INTO topics (name) VALUES (?)", name)
	return err
}

func (db *DB) ListTopics() ([]Topic, error) {
	rows, err := db.conn.Query("SELECT name, created_at FROM topics ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var topics []Topic
	for rows.Next() {
		var t Topic
		if err := rows.Scan(&t.Name, &t.CreatedAt); err != nil {
			return nil, err
		}
		topics = append(topics, t)
	}
	return topics, nil
}

func (db *DB) DeleteTopic(name string) error {
	_, err := db.conn.Exec("DELETE FROM topics WHERE name = ?", name)
	return err
}

// --- Queues ---

func (db *DB) CreateQueue(name string) error {
	_, err := db.conn.Exec("INSERT OR IGNORE INTO queues (name) VALUES (?)", name)
	return err
}

func (db *DB) ListQueues() ([]Queue, error) {
	rows, err := db.conn.Query(`
		SELECT q.name, q.created_at, COUNT(m.id) as depth
		FROM queues q
		LEFT JOIN messages m ON m.queue_name = q.name AND m.acked_at IS NULL
		GROUP BY q.name
		ORDER BY q.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var queues []Queue
	for rows.Next() {
		var q Queue
		if err := rows.Scan(&q.Name, &q.CreatedAt, &q.Depth); err != nil {
			return nil, err
		}
		queues = append(queues, q)
	}
	return queues, nil
}

func (db *DB) DeleteQueue(name string) error {
	_, err := db.conn.Exec("DELETE FROM queues WHERE name = ?", name)
	return err
}

// --- Subscriptions ---

func (db *DB) Subscribe(topicName, queueName string) (int64, error) {
	result, err := db.conn.Exec(
		"INSERT OR IGNORE INTO subscriptions (topic_name, queue_name) VALUES (?, ?)",
		topicName, queueName)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (db *DB) Unsubscribe(id int64) error {
	_, err := db.conn.Exec("DELETE FROM subscriptions WHERE id = ?", id)
	return err
}

func (db *DB) ListSubscriptions() ([]Subscription, error) {
	rows, err := db.conn.Query("SELECT id, topic_name, queue_name, created_at FROM subscriptions")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []Subscription
	for rows.Next() {
		var s Subscription
		if err := rows.Scan(&s.ID, &s.TopicName, &s.QueueName, &s.CreatedAt); err != nil {
			return nil, err
		}
		subs = append(subs, s)
	}
	return subs, nil
}

// --- Messages ---

// Publish sends a message to a topic. It's fanned out to all subscribed queues.
func (db *DB) Publish(topicName, body, fromAgent, priority string) (int, error) {
	if priority == "" {
		priority = "normal"
	}
	msgID := fmt.Sprintf("msg-%d", time.Now().UnixNano())

	// Get subscribed queues
	rows, err := db.conn.Query(
		"SELECT queue_name FROM subscriptions WHERE topic_name = ?", topicName)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var queues []string
	for rows.Next() {
		var q string
		rows.Scan(&q)
		queues = append(queues, q)
	}

	// Insert a message copy for each subscribed queue
	for i, queueName := range queues {
		id := fmt.Sprintf("%s-%d", msgID, i)
		_, err := db.conn.Exec(
			`INSERT INTO messages (id, topic_name, queue_name, body, from_agent, priority)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			id, topicName, queueName, body, fromAgent, priority)
		if err != nil {
			return i, fmt.Errorf("publishing to queue %s: %w", queueName, err)
		}
	}
	return len(queues), nil
}

// SendDirect sends a message directly to a queue (no topic routing).
func (db *DB) SendDirect(queueName, body, fromAgent, priority string) (string, error) {
	if priority == "" {
		priority = "normal"
	}
	id := fmt.Sprintf("msg-%d", time.Now().UnixNano())
	_, err := db.conn.Exec(
		`INSERT INTO messages (id, queue_name, body, from_agent, priority)
		 VALUES (?, ?, ?, ?, ?)`,
		id, queueName, body, fromAgent, priority)
	return id, err
}

// Receive reads unacked messages from a queue.
func (db *DB) Receive(queueName string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := db.conn.Query(`
		SELECT id, COALESCE(topic_name,''), queue_name, body, COALESCE(from_agent,''),
		       priority, created_at, read_at
		FROM messages
		WHERE queue_name = ? AND acked_at IS NULL
		ORDER BY created_at ASC
		LIMIT ?`, queueName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	now := time.Now()
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.TopicName, &m.QueueName, &m.Body,
			&m.FromAgent, &m.Priority, &m.CreatedAt, &m.ReadAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
		// Mark as read
		db.conn.Exec("UPDATE messages SET read_at = ? WHERE id = ?", now, m.ID)
	}
	return msgs, nil
}

// Ack acknowledges messages (removes from queue).
func (db *DB) Ack(messageIDs []string) error {
	now := time.Now()
	for _, id := range messageIDs {
		if _, err := db.conn.Exec(
			"UPDATE messages SET acked_at = ? WHERE id = ?", now, id); err != nil {
			return err
		}
	}
	return nil
}

// QueueDepth returns the number of unacked messages in a queue.
func (db *DB) QueueDepth(queueName string) (int, error) {
	var count int
	err := db.conn.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE queue_name = ? AND acked_at IS NULL",
		queueName).Scan(&count)
	return count, err
}
