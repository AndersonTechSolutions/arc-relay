// Package store — memory_extractions table access.
//
// memory_extractions is a provenance log: it records which messages went into
// which mem0 memory_ids so we can trace any distilled memory back to its
// source transcript chunks. mem0 itself is dedup-only (no UPDATE merge), so
// this table — not mem0 — is the source of truth for extraction history.
package store

import (
	"database/sql"
	"fmt"
)

// MemoryExtraction is one row in memory_extractions — one chunk's worth of
// extraction call output. A session that produces N chunks gets N rows.
type MemoryExtraction struct {
	ID            int64
	SessionID     string
	ExtractedAt   float64 // unix epoch (seconds, fractional)
	ChunkIndex    int
	ChunkMsgUUIDs string // JSON array of source message UUIDs
	ChunkChars    int
	Mem0MemoryIDs string // JSON array of mem0 IDs returned
	Mem0Count     int
	Error         sql.NullString // null on success
}

// ExtractionStore reads/writes memory_extractions rows.
type ExtractionStore struct {
	db *DB
}

// NewExtractionStore mirrors the NewSessionMemoryStore + NewMessageStore pattern.
func NewExtractionStore(db *DB) *ExtractionStore {
	return &ExtractionStore{db: db}
}

const insertExtractionSQL = `
INSERT INTO memory_extractions
    (session_id, extracted_at, chunk_index, chunk_msg_uuids, chunk_chars,
     mem0_memory_ids, mem0_count, error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`

// Insert records one chunk's extraction call. ChunkMsgUUIDs and Mem0MemoryIDs
// must be valid JSON arrays — caller is responsible for marshaling.
func (s *ExtractionStore) Insert(e *MemoryExtraction) error {
	_, err := s.db.Exec(insertExtractionSQL,
		e.SessionID, e.ExtractedAt, e.ChunkIndex, e.ChunkMsgUUIDs,
		e.ChunkChars, e.Mem0MemoryIDs, e.Mem0Count, e.Error,
	)
	if err != nil {
		return fmt.Errorf("insert extraction: %w", err)
	}
	return nil
}

// ListBySession returns every extraction row for a session, oldest-first
// (chunk_index ascending). Used by the idempotency check in extractor.Extract.
func (s *ExtractionStore) ListBySession(sessionID string) ([]*MemoryExtraction, error) {
	rows, err := s.db.Query(`
SELECT id, session_id, extracted_at, chunk_index, chunk_msg_uuids, chunk_chars,
       mem0_memory_ids, mem0_count, error
FROM memory_extractions
WHERE session_id = ?
ORDER BY chunk_index ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list extractions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*MemoryExtraction
	for rows.Next() {
		e := &MemoryExtraction{}
		if err := rows.Scan(&e.ID, &e.SessionID, &e.ExtractedAt, &e.ChunkIndex,
			&e.ChunkMsgUUIDs, &e.ChunkChars, &e.Mem0MemoryIDs, &e.Mem0Count, &e.Error); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Stats returns global extraction counts for the dashboard.
//   - rows: total memory_extractions rows
//   - mems: SUM(mem0_count) — total mem0 memories produced
//   - lastAt: MAX(extracted_at) — when the last extraction call ran
func (s *ExtractionStore) Stats() (rows, mems int64, lastAt float64, err error) {
	err = s.db.QueryRow(`
SELECT COUNT(*), COALESCE(SUM(mem0_count),0), COALESCE(MAX(extracted_at),0)
FROM memory_extractions`).Scan(&rows, &mems, &lastAt)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("extraction stats: %w", err)
	}
	return rows, mems, lastAt, nil
}
