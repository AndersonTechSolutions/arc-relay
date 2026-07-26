package store

import (
	"fmt"
)

// MemorySession is one row in memory_sessions — metadata about a transcript file.
// One session_id maps 1:1 to one ~/.claude/projects/.../<uuid>.jsonl file.
type MemorySession struct {
	SessionID   string
	UserID      string
	ProjectDir  string
	FilePath    string
	FileMtime   float64
	IndexedAt   float64
	LastSeenAt  float64
	CustomTitle string
	Platform    string
	BytesSeen   int64
}

// SessionMemoryStore manages memory_sessions rows. Companion to MessageStore.
type SessionMemoryStore struct {
	db *DB
}

// NewSessionMemoryStore mirrors the NewMessageStore and NewServerStore patterns.
func NewSessionMemoryStore(db *DB) *SessionMemoryStore {
	return &SessionMemoryStore{db: db}
}

const upsertSessionSQL = `
INSERT INTO memory_sessions
    (session_id, user_id, project_dir, file_path, file_mtime, indexed_at,
     last_seen_at, custom_title, platform, bytes_seen)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id) DO UPDATE SET
    file_mtime   = excluded.file_mtime,
    last_seen_at = excluded.last_seen_at,
    bytes_seen   = excluded.bytes_seen,
    custom_title = COALESCE(NULLIF(excluded.custom_title, ''), memory_sessions.custom_title)
`

// Upsert is idempotent on session_id. On conflict it advances mtime/seen/bytes
// and preserves a previously-set CustomTitle when the incoming value is empty.
// user_id, project_dir, file_path, indexed_at, and platform are insert-only —
// changing them retroactively would silently rewrite history.
func (s *SessionMemoryStore) Upsert(m *MemorySession) error {
	_, err := s.db.Exec(upsertSessionSQL,
		m.SessionID, m.UserID, m.ProjectDir, m.FilePath, m.FileMtime,
		m.IndexedAt, m.LastSeenAt, nullableString(m.CustomTitle), m.Platform, m.BytesSeen,
	)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	return nil
}

// Get returns the row for sessionID, or sql.ErrNoRows if not present.
// The error is intentionally NOT wrapped — callers can use errors.Is.
func (s *SessionMemoryStore) Get(sessionID string) (*MemorySession, error) {
	row := s.db.QueryRow(`
SELECT session_id, user_id, project_dir, file_path, file_mtime, indexed_at,
       last_seen_at, COALESCE(custom_title, ''), platform, bytes_seen
FROM memory_sessions WHERE session_id = ?`, sessionID)
	var m MemorySession
	err := row.Scan(&m.SessionID, &m.UserID, &m.ProjectDir, &m.FilePath,
		&m.FileMtime, &m.IndexedAt, &m.LastSeenAt, &m.CustomTitle,
		&m.Platform, &m.BytesSeen)
	if err != nil {
		return nil, err // do not wrap so errors.Is(err, sql.ErrNoRows) works
	}
	return &m, nil
}

// ListByUser returns the most-recent-first sessions for userID, capped at limit.
// limit <= 0 or > 200 defaults to 50; max is 200.
func (s *SessionMemoryStore) ListByUser(userID string, limit int) ([]*MemorySession, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`
SELECT session_id, user_id, project_dir, file_path, file_mtime, indexed_at,
       last_seen_at, COALESCE(custom_title, ''), platform, bytes_seen
FROM memory_sessions
WHERE user_id = ?
ORDER BY last_seen_at DESC
LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*MemorySession
	for rows.Next() {
		var m MemorySession
		if err := rows.Scan(&m.SessionID, &m.UserID, &m.ProjectDir, &m.FilePath,
			&m.FileMtime, &m.IndexedAt, &m.LastSeenAt, &m.CustomTitle,
			&m.Platform, &m.BytesSeen); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// ProjectGroup summarizes one project_dir for the dashboard landing page.
// Used by GroupByProject — gives the user a clustered view instead of a flat
// session list.
type ProjectGroup struct {
	ProjectDir   string
	SessionCount int
	LastSeenAt   float64
	FirstSeenAt  float64
	TotalBytes   int64
}

// GroupByProject returns one row per project_dir for userID, sorted by most
// recent activity. limit <= 0 or > 200 defaults to 50; max is 200.
func (s *SessionMemoryStore) GroupByProject(userID string, limit int) ([]*ProjectGroup, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`
SELECT project_dir,
       COUNT(*)         AS session_count,
       MAX(last_seen_at) AS last_seen_at,
       MIN(file_mtime)  AS first_seen_at,
       SUM(bytes_seen)  AS total_bytes
FROM memory_sessions
WHERE user_id = ?
GROUP BY project_dir
ORDER BY last_seen_at DESC
LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("group by project: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*ProjectGroup
	for rows.Next() {
		g := &ProjectGroup{}
		if err := rows.Scan(&g.ProjectDir, &g.SessionCount, &g.LastSeenAt,
			&g.FirstSeenAt, &g.TotalBytes); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListByUserPaged returns sessions for userID with pagination + optional
// project_dir filter. Returns the page slice, the total count (for pagination
// UI), and an error. limit <= 0 or > 200 defaults to 25; max 200.
func (s *SessionMemoryStore) ListByUserPaged(userID, projectDir string, limit, offset int) ([]*MemorySession, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}
	args := []any{userID}
	where := "user_id = ?"
	if projectDir != "" {
		where += " AND project_dir = ?"
		args = append(args, projectDir)
	}

	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM memory_sessions WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count sessions: %w", err)
	}

	args = append(args, limit, offset)
	rows, err := s.db.Query(`
SELECT session_id, user_id, project_dir, file_path, file_mtime, indexed_at,
       last_seen_at, COALESCE(custom_title, ''), platform, bytes_seen
FROM memory_sessions
WHERE `+where+`
ORDER BY last_seen_at DESC
LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list sessions paged: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*MemorySession
	for rows.Next() {
		var m MemorySession
		if err := rows.Scan(&m.SessionID, &m.UserID, &m.ProjectDir, &m.FilePath,
			&m.FileMtime, &m.IndexedAt, &m.LastSeenAt, &m.CustomTitle,
			&m.Platform, &m.BytesSeen); err != nil {
			return nil, 0, err
		}
		out = append(out, &m)
	}
	return out, total, rows.Err()
}

// CountMessages returns the number of memory_messages rows for a session.
// Used by the dashboard session-detail header without scanning the full body.
func (s *SessionMemoryStore) CountMessages(sessionID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM memory_messages WHERE session_id = ?`,
		sessionID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count messages: %w", err)
	}
	return n, nil
}

// Touch advances the watermark fields (mtime, last_seen_at, bytes_seen) without
// rewriting any metadata. Returns nil for a missing session (no-op semantics) —
// the watcher's flow always Upserts before Touching, but a transient gap should
// not crash it.
func (s *SessionMemoryStore) Touch(sessionID string, mtime float64, bytes int64) error {
	_, err := s.db.Exec(`
UPDATE memory_sessions
SET file_mtime = ?, last_seen_at = ?, bytes_seen = ?
WHERE session_id = ?`, mtime, mtime, bytes, sessionID)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// MarkExtracted stamps last_extracted_at on a session. Cron + on-demand both
// call this on a successful (or partial) Extract() pass; the column gates
// whether ListStaleForExtraction picks the session up again.
func (s *SessionMemoryStore) MarkExtracted(sessionID string, ts float64) error {
	_, err := s.db.Exec(
		`UPDATE memory_sessions SET last_extracted_at = ? WHERE session_id = ?`,
		ts, sessionID,
	)
	if err != nil {
		return fmt.Errorf("mark extracted: %w", err)
	}
	return nil
}

// ListStaleForExtraction returns up to `limit` session IDs that are eligible
// for the cron extraction backstop:
//   - last_seen_at older than 1 hour ago (don't compete with watcher quiescence)
//   - last_extracted_at is NULL OR strictly less than last_seen_at
//
// Ordered by last_seen_at DESC so newer-stale sessions extract first.
func (s *SessionMemoryStore) ListStaleForExtraction(limit int) ([]string, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.Query(`
SELECT session_id
FROM memory_sessions
WHERE last_seen_at < (strftime('%s','now') - 3600)
  AND (last_extracted_at IS NULL OR last_extracted_at < last_seen_at)
ORDER BY last_seen_at DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list stale for extraction: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
