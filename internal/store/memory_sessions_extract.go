package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ExtractionState is the per-session bookkeeping the extractor reads before
// a pass (migration 003 columns). It is deliberately separate from
// MemorySession so the dashboard's session row stays small.
type ExtractionState struct {
	SessionID             string
	Platform              string
	IsSubagent            bool
	ExtractedThroughMsgID sql.NullInt64
	ExtractAttempts       int
	LastExtractAttemptAt  sql.NullFloat64
	ExtractPoisonedAt     sql.NullFloat64
	LastActivityAt        sql.NullFloat64
	LastIngestedAt        sql.NullFloat64
}

// GetExtractionState returns the extraction bookkeeping for sessionID, or
// sql.ErrNoRows (unwrapped) when the session does not exist.
func (s *SessionMemoryStore) GetExtractionState(sessionID string) (*ExtractionState, error) {
	st := &ExtractionState{}
	err := s.db.QueryRow(`
SELECT session_id, platform, is_subagent, extracted_through_msg_id, extract_attempts,
       last_extract_attempt_at, extract_poisoned_at, last_activity_at, last_ingested_at
FROM memory_sessions WHERE session_id = ?`, sessionID).Scan(
		&st.SessionID, &st.Platform, &st.IsSubagent, &st.ExtractedThroughMsgID, &st.ExtractAttempts,
		&st.LastExtractAttemptAt, &st.ExtractPoisonedAt, &st.LastActivityAt, &st.LastIngestedAt)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// EligibilityConfig is the automatic-extraction gate (spec §4.1 P0-S4
// `eligible_auto`). One predicate, evaluated at enqueue, at dequeue and by
// cron, so no trigger path can bypass it.
type EligibilityConfig struct {
	// Platforms that may be extracted automatically. Empty means none.
	Platforms []string
	// MaxAge bounds last_activity_at; sessions with no parseable activity
	// timestamp are never eligible automatically.
	MaxAge time.Duration
	// QuietPeriod since last_ingested_at before a session is considered
	// settled. NULL (never ingested since migration 003) counts as quiet.
	QuietPeriod time.Duration
	// Backoff after failed attempts 1, 2 and 3+.
	Backoff [3]time.Duration
}

// DefaultEligibilityConfig returns the spec defaults: claude-code only,
// 30 days, 60 s quiet, 5 m / 30 m / 2 h backoff.
func DefaultEligibilityConfig() EligibilityConfig {
	return EligibilityConfig{
		Platforms:   []string{"claude-code"},
		MaxAge:      30 * 24 * time.Hour,
		QuietPeriod: 60 * time.Second,
		Backoff:     [3]time.Duration{5 * time.Minute, 30 * time.Minute, 2 * time.Hour},
	}
}

// eligibleAutoWhere is the SQL form of the predicate. The alias `s` is
// memory_sessions. Placeholders, in order: platforms..., now-MaxAge,
// now-QuietPeriod, backoff1, backoff2, backoff3, now.
func (cfg EligibilityConfig) where(now float64) (string, []any) {
	var args []any
	in := "NULL"
	if len(cfg.Platforms) > 0 {
		in = strings.TrimSuffix(strings.Repeat("?,", len(cfg.Platforms)), ",")
		for _, p := range cfg.Platforms {
			args = append(args, p)
		}
	}
	args = append(args,
		now-cfg.MaxAge.Seconds(),
		now-cfg.QuietPeriod.Seconds(),
		cfg.Backoff[0].Seconds(), cfg.Backoff[1].Seconds(), cfg.Backoff[2].Seconds(),
		now,
	)
	where := `
    s.is_subagent = 0
AND s.platform IN (` + in + `)
AND s.last_activity_at IS NOT NULL AND s.last_activity_at >= ?
AND (s.last_ingested_at IS NULL OR s.last_ingested_at < ?)
AND EXISTS (SELECT 1 FROM memory_messages m
            WHERE m.session_id = s.session_id
              AND m.id > COALESCE(s.extracted_through_msg_id, 0))
AND (s.extract_attempts = 0 OR s.last_extract_attempt_at IS NULL
     OR s.last_extract_attempt_at
        + CASE s.extract_attempts WHEN 1 THEN ? WHEN 2 THEN ? ELSE ? END <= ?)`
	return where, args
}

// ListEligibleForAutoExtraction returns up to limit session IDs that pass
// the automatic gate at `now`, least recently attempted first so a stable
// head of active sessions cannot starve older eligible ones.
func (s *SessionMemoryStore) ListEligibleForAutoExtraction(now float64, cfg EligibilityConfig, limit int) ([]string, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	where, args := cfg.where(now)
	args = append(args, limit)
	rows, err := s.db.Query(`
SELECT s.session_id FROM memory_sessions s
WHERE `+where+`
ORDER BY COALESCE(s.last_extract_attempt_at, 0) ASC, s.last_activity_at DESC
LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list eligible: %w", err)
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

// IsEligibleForAutoExtraction evaluates the same predicate for one session.
func (s *SessionMemoryStore) IsEligibleForAutoExtraction(sessionID string, now float64, cfg EligibilityConfig) (bool, error) {
	where, args := cfg.where(now)
	args = append(args, sessionID)
	var n int
	if err := s.db.QueryRow(`
SELECT COUNT(*) FROM memory_sessions s
WHERE `+where+`
AND s.session_id = ?`, args...).Scan(&n); err != nil {
		return false, fmt.Errorf("is eligible: %w", err)
	}
	return n > 0, nil
}

// BeginExtractAttempt stamps last_extract_attempt_at; the backoff clock
// starts here, not at completion, so a slow pass does not shorten it.
func (s *SessionMemoryStore) BeginExtractAttempt(sessionID string, now float64) error {
	_, err := s.db.Exec(`UPDATE memory_sessions SET last_extract_attempt_at = ? WHERE session_id = ?`, now, sessionID)
	if err != nil {
		return fmt.Errorf("begin extract attempt: %w", err)
	}
	return nil
}

// CompleteExtractPass advances the watermark to throughID after every chunk
// in the pass succeeded (or the range filtered to nothing) and clears the
// attempt counter.
func (s *SessionMemoryStore) CompleteExtractPass(sessionID string, throughID int64, now float64) error {
	_, err := s.db.Exec(`
UPDATE memory_sessions
SET extracted_through_msg_id = ?, last_extracted_at = ?, extract_attempts = 0
WHERE session_id = ?`, throughID, now, sessionID)
	if err != nil {
		return fmt.Errorf("complete extract pass: %w", err)
	}
	return nil
}

// FailExtractPass increments the attempt counter (the watermark stays put)
// and returns the new count so the caller can decide whether to poison.
func (s *SessionMemoryStore) FailExtractPass(sessionID string) (int, error) {
	var n int
	err := s.db.QueryRow(`
UPDATE memory_sessions SET extract_attempts = extract_attempts + 1
WHERE session_id = ? RETURNING extract_attempts`, sessionID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("fail extract pass: %w", err)
	}
	return n, nil
}

// PoisonExtractRange gives up on the range ending at throughID: the
// watermark skips past it, the attempt counter resets, and
// extract_poisoned_at records that a range was abandoned. Later messages in
// the session stay eligible.
func (s *SessionMemoryStore) PoisonExtractRange(sessionID string, throughID int64, now float64) error {
	_, err := s.db.Exec(`
UPDATE memory_sessions
SET extracted_through_msg_id = ?, extract_poisoned_at = ?, extract_attempts = 0
WHERE session_id = ?`, throughID, now, sessionID)
	if err != nil {
		return fmt.Errorf("poison extract range: %w", err)
	}
	return nil
}

// CountPoisoned returns how many sessions have an abandoned range, for stats.
func (s *SessionMemoryStore) CountPoisoned() (int64, error) {
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memory_sessions WHERE extract_poisoned_at IS NOT NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count poisoned: %w", err)
	}
	return n, nil
}
