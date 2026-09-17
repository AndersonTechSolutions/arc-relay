// Package memory orchestrates transcript ingestion: it routes JSONL through
// the appropriate platform parser and persists the resulting rows.
package memory

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/comma-compliance/arc-relay/internal/memory/parser"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// IngestRequest is the wire shape posted by the watcher (and by tests).
// UserID is intentionally NOT in this struct — the handler derives it from the
// authenticated user in context. Trusting a client-supplied user_id would be a
// security hole (any API key holder could write into another user's memory).
type IngestRequest struct {
	SessionID  string  `json:"session_id"`
	ProjectDir string  `json:"project_dir"`
	FilePath   string  `json:"file_path"`
	FileMtime  float64 `json:"file_mtime"`
	BytesSeen  int64   `json:"bytes_seen"`
	Platform   string  `json:"platform"`
	// JSONL is the raw transcript bytes. Go's encoding/json marshals []byte as
	// base64 on the wire; the watcher and the handler both rely on that default.
	JSONL []byte `json:"jsonl"`

	// Source identity, insert-only (Phase 3 sources fill these; the Claude
	// watcher may leave them empty).
	RawCwd                string `json:"raw_cwd,omitempty"`
	ProjectKey            string `json:"project_key,omitempty"`
	IsSubagent            bool   `json:"is_subagent,omitempty"`
	SourceParentSessionID string `json:"source_parent_session_id,omitempty"`
	// ClientCounters are watcher-side diagnostics (skipped_records, ...)
	// persisted per session and aggregated by Stats. Replaced on every ingest
	// that sends them.
	ClientCounters map[string]int64 `json:"client_counters,omitempty"`
}

// IngestResponse is returned to the caller on success.
type IngestResponse struct {
	// MessagesAdded counts rows actually stored; replayed duplicates are not
	// counted, so a watcher re-sending a chunk sees 0.
	MessagesAdded int   `json:"messages_added"`
	EventsAdded   int   `json:"events_added"`
	BytesSeen     int64 `json:"bytes_seen"`
}

// Service is the seam between HTTP/MCP handlers and the storage layer.
type Service struct {
	sessions *store.SessionMemoryStore
	messages *store.MessageStore
	now      func() time.Time // injectable for tests
}

// NewService creates a Service backed by the given stores.
func NewService(sessions *store.SessionMemoryStore, messages *store.MessageStore) *Service {
	return &Service{sessions: sessions, messages: messages, now: time.Now}
}

// Ingest parses a JSONL chunk under the calling user's identity and persists
// rows. Idempotent: a message whose (session_id, uuid) already exists is
// dropped by the unique index from migration 003. Atomic: the ownership
// check, the session upsert, the message inserts, and the ingest stamps
// commit together, so a session row never advertises messages that were
// rolled back and another user's session_id is never written into
// (store.ErrForeignSession).
func (s *Service) Ingest(userID string, req *IngestRequest) (*IngestResponse, error) {
	if req.SessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	if req.Platform == "" {
		return nil, fmt.Errorf("platform is required")
	}
	p := parser.Get(req.Platform)
	if p == nil {
		return nil, fmt.Errorf("unknown platform %q", req.Platform)
	}

	// Parse outside the transaction: a parse failure must not hold a write
	// lock, and it writes nothing.
	msgs, events, err := p.Parse(bytes.NewReader(req.JSONL))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	for _, m := range msgs {
		m.SessionID = req.SessionID
		m.Platform = req.Platform
	}
	activity := newestTimestamp(msgs)
	counters := ""
	if req.ClientCounters != nil {
		if b, err := json.Marshal(req.ClientCounters); err == nil {
			counters = string(b)
		}
	}

	tx, err := s.messages.DB().Begin()
	if err != nil {
		return nil, fmt.Errorf("begin ingest: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	owner, exists, err := s.sessions.OwnerTx(tx, req.SessionID)
	if err != nil {
		return nil, err
	}
	if exists && owner != userID {
		return nil, store.ErrForeignSession
	}

	if err := s.sessions.UpsertTx(tx, &store.MemorySession{
		SessionID:             req.SessionID,
		UserID:                userID,
		ProjectDir:            req.ProjectDir,
		FilePath:              req.FilePath,
		FileMtime:             req.FileMtime,
		IndexedAt:             req.FileMtime,
		LastSeenAt:            req.FileMtime,
		Platform:              req.Platform,
		BytesSeen:             req.BytesSeen,
		RawCwd:                req.RawCwd,
		ProjectKey:            req.ProjectKey,
		IsSubagent:            req.IsSubagent,
		SourceParentSessionID: req.SourceParentSessionID,
	}); err != nil {
		return nil, fmt.Errorf("upsert session: %w", err)
	}

	stored, err := s.messages.BulkInsertTx(tx, msgs)
	if err != nil {
		return nil, fmt.Errorf("bulk insert: %w", err)
	}
	if err := s.sessions.RecordIngestTx(tx, req.SessionID, float64(s.now().UnixNano())/1e9, activity, counters); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit ingest: %w", err)
	}
	// CompactEvents persistence is Phase 4 (LLM observation layer). v1 returns
	// the count for diagnostics but does not write a memory_compact_events row.
	return &IngestResponse{
		MessagesAdded: stored,
		EventsAdded:   len(events),
		BytesSeen:     req.BytesSeen,
	}, nil
}

// SessionOwnedBy reports whether sessionID exists and belongs to userID. It
// reads one column; use it for authorization instead of loading messages.
func (s *Service) SessionOwnedBy(userID, sessionID string) (bool, error) {
	owner, exists, err := s.sessions.Owner(sessionID)
	if err != nil {
		return false, err
	}
	return exists && owner == userID, nil
}

// timestampLayouts are tried in order when deriving last_activity_at.
// Claude Code writes RFC 3339 with milliseconds and 'Z'; Codex writes RFC
// 3339 with an offset; the normalized Phase 3 wire shape is plain RFC 3339.
var timestampLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.000Z07:00",
	"2006-01-02 15:04:05",
}

func parseTimestamp(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	for _, layout := range timestampLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return float64(t.UnixNano()) / 1e9, true
		}
	}
	return 0, false
}

// newestTimestamp returns the newest parseable message timestamp, or nil when
// none parse, in which case last_activity_at is left unchanged.
func newestTimestamp(msgs []*store.Message) *float64 {
	var newest *float64
	for _, m := range msgs {
		ts, ok := parseTimestamp(m.Timestamp)
		if !ok {
			continue
		}
		if newest == nil || ts > *newest {
			v := ts
			newest = &v
		}
	}
	return newest
}

// Search routes the query through FTS5 BM25 by default, falling back to a Go
// regexp scan when the query contains regex metacharacters and is not wrapped
// in double quotes (escape hatch for users who want a literal match).
//
// Three-tier escalation handles FTS5 syntax errors (e.g. hyphens parsed as NOT,
// colons as column scopes):
//  1. Try FTS5 with the raw query.
//  2. On error, retry as a quoted phrase ("...") which treats all metacharacters
//     literally. Embedded double quotes are doubled per FTS5 phrase-string rules.
//  3. On second error, fall back to Go regex scan. If that also fails, return the
//     original FTS5 error — it is the most informative.
func (s *Service) Search(userID, query string, opts store.SearchOpts) ([]*store.SearchHit, error) {
	if hasRegexMeta(query) {
		return s.messages.SearchRegex(userID, query, opts)
	}
	// First try: raw query as FTS5 input.
	hits, err := s.messages.Search(userID, query, opts)
	if err == nil {
		return hits, nil
	}
	// Second try: wrap as a phrase (handles hyphens, colons, and other FTS5
	// metacharacters that the user didn't intend as syntax).
	quoted := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
	slog.Debug("memory search: FTS5 raw query failed, retrying as phrase",
		"query", query, "quoted", quoted, "err", err)
	hits, err2 := s.messages.Search(userID, quoted, opts)
	if err2 == nil {
		return hits, nil
	}
	// Third try: regex fallback.
	slog.Debug("memory search: phrase retry also failed, falling back to regex",
		"query", query, "err", err2)
	hits, err3 := s.messages.SearchRegex(userID, query, opts)
	if err3 != nil {
		// Surface the ORIGINAL FTS5 error since it's the most informative.
		return nil, fmt.Errorf("fts5 + phrase + regex all failed; first error: %w", err)
	}
	return hits, nil
}

// SessionExtract returns the messages of one session, with a user-scope check
// (don't leak the existence of another user's session).
func (s *Service) SessionExtract(userID, sessionID string, fromEpoch int) ([]*store.Message, error) {
	sess, err := s.sessions.Get(sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("session not found")
		}
		return nil, fmt.Errorf("get session: %w", err)
	}
	if sess.UserID != userID {
		// Same error as missing — don't reveal existence to wrong user.
		return nil, fmt.Errorf("session not found")
	}
	return s.messages.GetSession(sessionID, fromEpoch)
}

// GetSessionWithMessages returns session metadata + messages for a single
// session, with the same user-scope check as SessionExtract (returns
// "session not found" for both missing and wrong-user cases). Used by
// the web detail page which needs both the header (project_dir, file_path,
// last_seen_at) AND the message body in one call.
func (s *Service) GetSessionWithMessages(userID, sessionID string, fromEpoch int) (*store.MemorySession, []*store.Message, error) {
	sess, err := s.sessions.Get(sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("session not found")
		}
		return nil, nil, fmt.Errorf("get session: %w", err)
	}
	if sess.UserID != userID {
		// Same error as missing — don't reveal existence to wrong user.
		return nil, nil, fmt.Errorf("session not found")
	}
	msgs, err := s.messages.GetSession(sessionID, fromEpoch)
	if err != nil {
		return nil, nil, err
	}
	return sess, msgs, nil
}

// Recent lists the calling user's most-recent sessions.
func (s *Service) Recent(userID string, limit int) ([]*store.MemorySession, error) {
	return s.sessions.ListByUser(userID, limit)
}

// RecentByProject groups the calling user's sessions by project_dir, sorted
// by most-recent activity. Used by the dashboard landing page.
func (s *Service) RecentByProject(userID string, limit int) ([]*store.ProjectGroup, error) {
	return s.sessions.GroupByProject(userID, limit)
}

// SessionsPaged returns one page of sessions with optional project_dir filter.
// Returns the page slice + total count (for pagination UI).
func (s *Service) SessionsPaged(userID, projectDir string, limit, offset int) ([]*store.MemorySession, int, error) {
	return s.sessions.ListByUserPaged(userID, projectDir, limit, offset)
}

// SessionMessageCount is a fast COUNT(*) for the session-detail header — lets
// the page render the "N messages" stat without re-scanning the body.
func (s *Service) SessionMessageCount(sessionID string) (int, error) {
	return s.sessions.CountMessages(sessionID)
}

// Stats is the diagnostic shape returned by HandleStats — global counts,
// not user-scoped (count != content; safe to surface).
type Stats struct {
	DBBytes  int64 `json:"db_bytes"`
	Sessions int64 `json:"sessions"`
	Messages int64 `json:"messages"`
	// LastIngestAt is the newest server receipt time (last_ingested_at).
	// Sessions ingested before migration 003 have no receipt time, so on a
	// deployment with no ingest since the migration this is 0.
	LastIngestAt float64 `json:"last_ingest_at"`
	// LastActivityAt is the newest message timestamp seen across sessions —
	// the old "last ingest" semantics (client-side time), kept for context.
	LastActivityAt float64 `json:"last_activity_at"`
	// Platforms lists the parsers this relay accepts.
	Platforms []string `json:"platforms"`
	// MessagesByPlatform counts stored rows per platform (indexed column).
	MessagesByPlatform map[string]int64 `json:"messages_by_platform"`
	// ClientCounters sums the watcher-side counters across sessions.
	ClientCounters map[string]int64 `json:"client_counters"`
}

// Stats returns DB-level counts + ingest timestamps + per-platform message
// counts + aggregated client counters. Used by `arc-sync memory stats` and
// the dashboard.
func (s *Service) Stats() (*Stats, error) {
	st := &Stats{
		Platforms:          parser.Platforms(),
		MessagesByPlatform: map[string]int64{},
		ClientCounters:     map[string]int64{},
	}
	if err := s.statsPlatformsAndCounters(st); err != nil {
		return nil, err
	}

	// page_count * page_size — the actual on-disk database size.
	if err := s.messages.DB().QueryRow(
		`SELECT (SELECT page_count FROM pragma_page_count) * (SELECT page_size FROM pragma_page_size)`,
	).Scan(&st.DBBytes); err != nil {
		return nil, fmt.Errorf("db bytes: %w", err)
	}
	if err := s.messages.DB().QueryRow(
		`SELECT count(*) FROM memory_sessions`,
	).Scan(&st.Sessions); err != nil {
		return nil, fmt.Errorf("sessions count: %w", err)
	}
	if err := s.messages.DB().QueryRow(
		`SELECT count(*) FROM memory_messages`,
	).Scan(&st.Messages); err != nil {
		return nil, fmt.Errorf("messages count: %w", err)
	}
	if err := s.messages.DB().QueryRow(
		`SELECT COALESCE(MAX(last_ingested_at), 0), COALESCE(MAX(last_activity_at), 0) FROM memory_sessions`,
	).Scan(&st.LastIngestAt, &st.LastActivityAt); err != nil {
		return nil, fmt.Errorf("last ingest: %w", err)
	}
	return st, nil
}

// statsPlatformsAndCounters fills MessagesByPlatform (one indexed GROUP BY)
// and sums every session's client_counters JSON blob into ClientCounters.
func (s *Service) statsPlatformsAndCounters(st *Stats) error {
	rows, err := s.messages.DB().Query(`SELECT platform, COUNT(*) FROM memory_messages GROUP BY platform`)
	if err != nil {
		return fmt.Errorf("messages by platform: %w", err)
	}
	for rows.Next() {
		var p string
		var n int64
		if err := rows.Scan(&p, &n); err != nil {
			_ = rows.Close()
			return err
		}
		st.MessagesByPlatform[p] = n
	}
	_ = rows.Close()

	crows, err := s.messages.DB().Query(`SELECT client_counters FROM memory_sessions WHERE client_counters IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("client counters: %w", err)
	}
	defer func() { _ = crows.Close() }()
	for crows.Next() {
		var blob string
		if err := crows.Scan(&blob); err != nil {
			return err
		}
		var m map[string]int64
		if json.Unmarshal([]byte(blob), &m) != nil {
			continue // a malformed blob is a client bug, not a stats outage
		}
		for k, v := range m {
			st.ClientCounters[k] += v
		}
	}
	return crows.Err()
}

// hasRegexMeta detects FTS5-incompatible characters that should route to the
// regex fallback. Quoted strings bypass detection so users can search for
// literal punctuation by quoting it.
func hasRegexMeta(q string) bool {
	if len(q) >= 2 && q[0] == '"' && q[len(q)-1] == '"' {
		return false
	}
	for _, r := range q {
		switch r {
		case '\\', '.', '*', '+', '?', '[', ']', '{', '}', '(', ')', '|', '^', '$':
			return true
		}
	}
	return false
}
