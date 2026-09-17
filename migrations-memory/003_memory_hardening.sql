-- migrations-memory/003_memory_hardening.sql
-- Phase 0 hardening (docs/superpowers/specs/2026-09-17-arc-relay-memory-phase3-design.md §4.1).
--
-- Measured on a copy of the 2026-09-17 production DB (700,046 rows, 1.75 GB):
-- DELETE 18s, FTS rebuild 94s, unique index 5s, COMMIT 13s. The relay is
-- down for the duration because migrations run inside store.Open.

-- 1. Remove the duplicate rows that the plain uuid index let through.
--    Keep the lowest id per (session_id, uuid); NULL uuids are never touched.
--    The AFTER DELETE trigger removes the matching FTS entries.
DELETE FROM memory_messages
WHERE uuid IS NOT NULL
  AND id NOT IN (
    SELECT MIN(id) FROM memory_messages
    WHERE uuid IS NOT NULL
    GROUP BY session_id, uuid
  );

-- 2. Rebuild the external-content FTS index so the post-state is correct
--    regardless of whether the delete trigger's 'delete' commands matched
--    the previous index contents.
INSERT INTO memory_messages_fts(memory_messages_fts) VALUES ('rebuild');

-- 3. Idempotent ingest: (session_id, uuid) is unique per session. A plain
--    (non-partial) unique index so that ON CONFLICT(session_id, uuid) can
--    target it; SQLite allows multiple NULL uuids under a unique index.
CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_messages_session_uuid
    ON memory_messages(session_id, uuid);

-- 4. Denormalize platform onto messages so per-platform counts are indexed.
--    Every existing row belongs to a claude-code session (the default), so
--    the UPDATE below touches zero rows today and stays cheap on the FTS
--    update trigger. It exists for correctness if a non-claude session
--    predates this migration on another deployment.
ALTER TABLE memory_messages ADD COLUMN platform TEXT NOT NULL DEFAULT 'claude-code';

UPDATE memory_messages
SET platform = (SELECT s.platform FROM memory_sessions s WHERE s.session_id = memory_messages.session_id)
WHERE session_id IN (SELECT session_id FROM memory_sessions WHERE platform <> 'claude-code');

CREATE INDEX IF NOT EXISTS idx_memory_messages_platform
    ON memory_messages(platform);

-- 5. Session columns.
--    raw_cwd / project_key / is_subagent / source_parent_session_id: source
--      identity for Phase 3 (insert-only on upsert).
--    last_activity_at: max parsed message timestamp (updated on every ingest).
--    last_ingested_at: server receipt time (updated on every ingest).
--    extracted_through_msg_id: highest memory_messages.id covered by a fully
--      successful extraction pass; NULL for every pre-existing session so the
--      first pass re-walks it with the coveredUUIDs guard preventing re-spend.
--    extract_attempts / last_extract_attempt_at / extract_poisoned_at: retry
--      bookkeeping for bounded passes.
--    client_counters: JSON blob of watcher-side counters, replaced per ingest.
ALTER TABLE memory_sessions ADD COLUMN raw_cwd TEXT;
ALTER TABLE memory_sessions ADD COLUMN project_key TEXT;
ALTER TABLE memory_sessions ADD COLUMN is_subagent INTEGER NOT NULL DEFAULT 0;
ALTER TABLE memory_sessions ADD COLUMN source_parent_session_id TEXT;
ALTER TABLE memory_sessions ADD COLUMN last_activity_at REAL;
ALTER TABLE memory_sessions ADD COLUMN last_ingested_at REAL;
ALTER TABLE memory_sessions ADD COLUMN extracted_through_msg_id INTEGER;
ALTER TABLE memory_sessions ADD COLUMN extract_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE memory_sessions ADD COLUMN last_extract_attempt_at REAL;
ALTER TABLE memory_sessions ADD COLUMN extract_poisoned_at REAL;
ALTER TABLE memory_sessions ADD COLUMN client_counters TEXT;

CREATE INDEX IF NOT EXISTS idx_memory_sessions_parent
    ON memory_sessions(source_parent_session_id);

-- 6. Backfill last_activity_at from message timestamps. unixepoch() accepts
--    RFC 3339 with a 'Z' or ±HH:MM suffix and returns NULL for anything it
--    cannot parse, which the age gate treats as "old" (ineligible).
UPDATE memory_sessions
SET last_activity_at = (
    SELECT MAX(unixepoch(m.timestamp)) FROM memory_messages m
    WHERE m.session_id = memory_sessions.session_id
);
