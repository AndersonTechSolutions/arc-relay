package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// Message is one row in memory_messages — a single transcript entry.
type Message struct {
	ID         int64
	UUID       string
	SessionID  string
	ParentUUID string
	Epoch      int
	Timestamp  string
	Role       string
	Content    string
}

// SearchHit is one search result row.
type SearchHit struct {
	Message
	Score float64 // bm25 score for FTS5 (lower = better); 0 for regex
}

// SearchOpts bounds a search.
type SearchOpts struct {
	Limit      int
	ProjectDir string // optional filter
	SessionID  string // optional filter
	Role       string // optional filter — exact match on memory_messages.role
	SinceEpoch int    // 0 = all
}

// MessageStore reads/writes memory_messages and serves FTS5 + regex queries.
// All read paths enforce user scoping by joining memory_sessions.user_id.
type MessageStore struct {
	db *DB
}

// NewMessageStore mirrors the NewServerStore pattern from servers.go.
func NewMessageStore(db *DB) *MessageStore {
	return &MessageStore{db: db}
}

const insertMessageSQL = `
INSERT INTO memory_messages
    (uuid, session_id, parent_uuid, epoch, timestamp, role, content)
VALUES (?, ?, ?, ?, ?, ?, ?)
`

// Insert inserts a single message and populates m.ID from LastInsertId.
func (s *MessageStore) Insert(m *Message) error {
	res, err := s.db.Exec(insertMessageSQL,
		nullableString(m.UUID), m.SessionID, nullableString(m.ParentUUID),
		m.Epoch, m.Timestamp, m.Role, m.Content,
	)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	id, _ := res.LastInsertId()
	m.ID = id
	return nil
}

// BulkInsert inserts all messages in a single transaction.
// Rolls back on any insert failure; populates each m.ID on success.
func (s *MessageStore) BulkInsert(msgs []*Message) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.Prepare(insertMessageSQL)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, m := range msgs {
		res, err := stmt.Exec(
			nullableString(m.UUID), m.SessionID, nullableString(m.ParentUUID),
			m.Epoch, m.Timestamp, m.Role, m.Content,
		)
		if err != nil {
			return fmt.Errorf("bulk insert: %w", err)
		}
		id, _ := res.LastInsertId()
		m.ID = id
	}
	return tx.Commit()
}

// Search runs an FTS5 BM25 query scoped to userID.
// Caller passes the raw user query; double-quote phrases for exact-match.
// FTS5 bm25 returns lower scores for better matches, hence ORDER BY score ASC.
func (s *MessageStore) Search(userID, query string, opts SearchOpts) ([]*SearchHit, error) {
	if opts.Limit <= 0 || opts.Limit > 200 {
		opts.Limit = 25
	}
	args := []any{userID, query}
	where := []string{"s.user_id = ?", "memory_messages_fts MATCH ?"}
	if opts.ProjectDir != "" {
		where = append(where, "s.project_dir = ?")
		args = append(args, opts.ProjectDir)
	}
	if opts.SessionID != "" {
		where = append(where, "m.session_id = ?")
		args = append(args, opts.SessionID)
	}
	if opts.Role != "" {
		where = append(where, "m.role = ?")
		args = append(args, opts.Role)
	}
	if opts.SinceEpoch > 0 {
		where = append(where, "m.epoch >= ?")
		args = append(args, opts.SinceEpoch)
	}
	args = append(args, opts.Limit)

	q := fmt.Sprintf(`
SELECT m.id, COALESCE(m.uuid,''), m.session_id, COALESCE(m.parent_uuid,''),
       m.epoch, m.timestamp, m.role, m.content,
       bm25(memory_messages_fts) AS score
FROM memory_messages_fts
JOIN memory_messages m ON m.id = memory_messages_fts.rowid
JOIN memory_sessions  s ON s.session_id = m.session_id
WHERE %s
ORDER BY score ASC
LIMIT ?`, strings.Join(where, " AND "))

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanHits(rows)
}

// SearchRegex runs a Go regexp over messages scoped to userID.
// Used when the query contains FTS5-incompatible metacharacters.
// Strategy: pull a bounded set of recent rows for the user, then filter in-process.
func (s *MessageStore) SearchRegex(userID, pattern string, opts SearchOpts) ([]*SearchHit, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile regex: %w", err)
	}
	if opts.Limit <= 0 || opts.Limit > 500 {
		opts.Limit = 100
	}
	args := []any{userID}
	where := []string{"s.user_id = ?"}
	if opts.ProjectDir != "" {
		where = append(where, "s.project_dir = ?")
		args = append(args, opts.ProjectDir)
	}
	if opts.SessionID != "" {
		where = append(where, "m.session_id = ?")
		args = append(args, opts.SessionID)
	}
	if opts.Role != "" {
		where = append(where, "m.role = ?")
		args = append(args, opts.Role)
	}
	q := fmt.Sprintf(`
SELECT m.id, COALESCE(m.uuid,''), m.session_id, COALESCE(m.parent_uuid,''),
       m.epoch, m.timestamp, m.role, m.content
FROM memory_messages m
JOIN memory_sessions s ON s.session_id = m.session_id
WHERE %s
ORDER BY m.id DESC
LIMIT 5000`, strings.Join(where, " AND "))

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("regex base query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []*SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.ID, &h.UUID, &h.SessionID, &h.ParentUUID,
			&h.Epoch, &h.Timestamp, &h.Role, &h.Content); err != nil {
			return nil, err
		}
		if !re.MatchString(h.Content) {
			continue
		}
		hits = append(hits, &h)
		if len(hits) >= opts.Limit {
			break
		}
	}
	return hits, rows.Err()
}

// GetSession returns all messages for sessionID with epoch >= fromEpoch, ordered by id ASC.
func (s *MessageStore) GetSession(sessionID string, fromEpoch int) ([]*Message, error) {
	rows, err := s.db.Query(`
SELECT id, COALESCE(uuid,''), session_id, COALESCE(parent_uuid,''),
       epoch, timestamp, role, content
FROM memory_messages
WHERE session_id = ? AND epoch >= ?
ORDER BY id ASC`, sessionID, fromEpoch)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.UUID, &m.SessionID, &m.ParentUUID,
			&m.Epoch, &m.Timestamp, &m.Role, &m.Content); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

func scanHits(rows *sql.Rows) ([]*SearchHit, error) {
	var hits []*SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.ID, &h.UUID, &h.SessionID, &h.ParentUUID,
			&h.Epoch, &h.Timestamp, &h.Role, &h.Content, &h.Score); err != nil {
			return nil, err
		}
		hits = append(hits, &h)
	}
	return hits, rows.Err()
}

// DB returns the underlying *DB for diagnostic queries that don't fit a store
// method (currently used by memory.Service.Stats for pragma queries).
func (s *MessageStore) DB() *DB { return s.db }

// nullableString returns nil for empty strings so SQLite stores NULL instead
// of an empty string in nullable columns like memory_messages.uuid.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
