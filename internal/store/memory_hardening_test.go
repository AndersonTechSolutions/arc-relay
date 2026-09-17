package store

import (
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	migrationsmemory "github.com/comma-compliance/arc-relay/migrations-memory"
)

// migrationsBefore returns an fs.FS holding only the migrations that sort
// strictly before `cutoff`, so a test can build a database in the pre-cutoff
// schema, seed it, and then reopen it with the full FS to exercise `cutoff`
// against real data instead of an empty table.
func migrationsBefore(t *testing.T, cutoff string) fs.FS {
	t.Helper()
	entries, err := fs.ReadDir(migrationsmemory.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	m := fstest.MapFS{}
	for _, e := range entries {
		if e.Name() >= cutoff {
			continue
		}
		b, err := fs.ReadFile(migrationsmemory.FS, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		m[e.Name()] = &fstest.MapFile{Data: b}
	}
	if len(m) == 0 {
		t.Fatalf("no migrations before %s", cutoff)
	}
	return m
}

func seedSession(t *testing.T, db *DB, id, platform string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO memory_sessions
		(session_id, user_id, project_dir, file_path, file_mtime, indexed_at, last_seen_at, platform)
		VALUES (?, 'u1', '/p', '/f', 1, 1, 1, ?)`, id, platform)
	if err != nil {
		t.Fatal(err)
	}
}

func rawInsert(t *testing.T, db *DB, uuid any, session, ts, content string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO memory_messages (uuid, session_id, epoch, timestamp, role, content)
		VALUES (?, ?, 0, ?, 'user', ?)`, uuid, session, ts, content)
	if err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, db *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// TestMigration003Dedup builds a pre-003 database with the exact duplicate
// shapes seen in production (same session+uuid, same uuid across sessions,
// NULL uuids), applies 003, and checks what the spec promises: only
// same-session duplicates go, the FTS index stays consistent, the unique
// index exists, and last_activity_at is backfilled from parseable timestamps.
func TestMigration003Dedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")

	db, err := Open(path, migrationsBefore(t, "003_"))
	if err != nil {
		t.Fatalf("open pre-003: %v", err)
	}
	seedSession(t, db, "s1", "claude-code")
	seedSession(t, db, "s2", "claude-code")
	seedSession(t, db, "s3", "claude-code")
	// s1: one uuid stored three times (prod shape), another once.
	rawInsert(t, db, "aaa", "s1", "2026-09-17T04:31:02.123Z", "alpha dup")
	rawInsert(t, db, "aaa", "s1", "2026-09-17T04:31:02.123Z", "alpha dup")
	rawInsert(t, db, "aaa", "s1", "2026-09-17T04:31:02.123Z", "alpha dup")
	rawInsert(t, db, "bbb", "s1", "2026-09-17T05:00:00Z", "bravo")
	// s2: same uuid as s1 (resumed session) — must survive.
	rawInsert(t, db, "aaa", "s2", "2026-09-16T10:00:00+02:00", "alpha other session")
	// s3: two NULL-uuid rows and one unparseable timestamp — never deduped.
	rawInsert(t, db, nil, "s3", "not a timestamp", "null one")
	rawInsert(t, db, nil, "s3", "also not", "null two")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path, migrationsmemory.FS)
	if err != nil {
		t.Fatalf("open with 003: %v", err)
	}
	defer func() { _ = db.Close() }()

	if got := count(t, db, `SELECT COUNT(*) FROM memory_messages WHERE session_id='s1' AND uuid='aaa'`); got != 1 {
		t.Errorf("s1/aaa rows after dedup = %d, want 1", got)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM memory_messages WHERE uuid='aaa'`); got != 2 {
		t.Errorf("uuid aaa across sessions = %d, want 2 (cross-session copies are legitimate)", got)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM memory_messages WHERE uuid IS NULL`); got != 2 {
		t.Errorf("NULL-uuid rows = %d, want 2 (never deduplicated)", got)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM memory_messages`); got != 5 {
		t.Errorf("total rows = %d, want 5", got)
	}

	// Content-aware FTS integrity check (SQLite ≥ 3.41): errors if the index
	// and the content table disagree.
	if _, err := db.Exec(`INSERT INTO memory_messages_fts(memory_messages_fts, rank) VALUES('integrity-check', 1)`); err != nil {
		t.Errorf("FTS integrity-check after migration: %v", err)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM memory_messages_fts WHERE memory_messages_fts MATCH 'alpha'`); got != 2 {
		t.Errorf("FTS hits for 'alpha' = %d, want 2 (one per surviving row)", got)
	}

	var idx string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_memory_messages_session_uuid'`).Scan(&idx); err != nil {
		t.Errorf("unique index missing: %v", err)
	}

	// last_activity_at backfill: max parseable timestamp; NULL when none parse.
	var s1, s2 float64
	var s3 *float64
	if err := db.QueryRow(`SELECT last_activity_at FROM memory_sessions WHERE session_id='s1'`).Scan(&s1); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT last_activity_at FROM memory_sessions WHERE session_id='s2'`).Scan(&s2); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT last_activity_at FROM memory_sessions WHERE session_id='s3'`).Scan(&s3); err != nil {
		t.Fatal(err)
	}
	if s1 != 1789621200 { // 2026-09-17T05:00:00Z
		t.Errorf("s1 last_activity_at = %v, want 1789621200", s1)
	}
	if s2 != 1789545600 { // 2026-09-16T10:00:00+02:00 = 08:00Z
		t.Errorf("s2 last_activity_at = %v, want 1789545600 (offset honoured)", s2)
	}
	if s3 != nil {
		t.Errorf("s3 last_activity_at = %v, want NULL (unparseable timestamps)", *s3)
	}

	// Every pre-existing session starts with no extraction watermark.
	if got := count(t, db, `SELECT COUNT(*) FROM memory_sessions WHERE extracted_through_msg_id IS NOT NULL`); got != 0 {
		t.Errorf("sessions with a watermark after migration = %d, want 0", got)
	}
	// platform denormalized onto messages, default claude-code.
	if got := count(t, db, `SELECT COUNT(*) FROM memory_messages WHERE platform='claude-code'`); got != 5 {
		t.Errorf("messages with platform=claude-code = %d, want 5", got)
	}
}

// TestBulkInsertIdempotent covers the ON CONFLICT(session_id, uuid) contract:
// re-sending the same chunk stores nothing and reports 0, the same uuid in
// another session is a distinct row, and NULL-uuid rows are never collapsed.
func TestBulkInsertIdempotent(t *testing.T) {
	db, err := Open(":memory:", migrationsmemory.FS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	seedSession(t, db, "s1", "codex")
	seedSession(t, db, "s2", "codex")
	ms := NewMessageStore(db)

	batch := func(session string) []*Message {
		return []*Message{
			{UUID: "u1", SessionID: session, Timestamp: "2026-09-17T00:00:00Z", Role: "user", Content: "one", Platform: "codex"},
			{UUID: "u2", SessionID: session, Timestamp: "2026-09-17T00:00:01Z", Role: "assistant", Content: "two", Platform: "codex"},
			{UUID: "", SessionID: session, Timestamp: "2026-09-17T00:00:02Z", Role: "tool", Content: "no uuid", Platform: "codex"},
		}
	}

	n, err := ms.BulkInsert(batch("s1"))
	if err != nil || n != 3 {
		t.Fatalf("first insert: n=%d err=%v, want 3", n, err)
	}
	n, err = ms.BulkInsert(batch("s1"))
	if err != nil || n != 1 {
		t.Fatalf("replay: n=%d err=%v, want 1 (only the NULL-uuid row is new)", n, err)
	}
	n, err = ms.BulkInsert(batch("s2"))
	if err != nil || n != 3 {
		t.Fatalf("other session: n=%d err=%v, want 3", n, err)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM memory_messages`); got != 7 {
		t.Errorf("rows = %d, want 7", got)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM memory_messages WHERE platform='codex'`); got != 7 {
		t.Errorf("platform column not stored: %d codex rows, want 7", got)
	}

	// A duplicate must not report a stale LastInsertId as its own.
	dup := &Message{UUID: "u1", SessionID: "s1", Timestamp: "x", Role: "user", Content: "one"}
	stored, err := ms.Insert(dup)
	if err != nil || stored || dup.ID != 0 {
		t.Errorf("Insert duplicate: stored=%v id=%d err=%v, want false/0/nil", stored, dup.ID, err)
	}

	// FTS stays consistent through skipped conflicts.
	if _, err := db.Exec(`INSERT INTO memory_messages_fts(memory_messages_fts, rank) VALUES('integrity-check', 1)`); err != nil {
		t.Errorf("FTS integrity-check: %v", err)
	}
}
