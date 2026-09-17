package store

import (
	"testing"

	migrationsmemory "github.com/comma-compliance/arc-relay/migrations-memory"
)

// seedTestSession is a test-only shortcut that calls SessionMemoryStore.Upsert
// to seed the parent row required by memory_messages's foreign key.
func seedTestSession(t *testing.T, db *DB, sessionID, userID string) {
	t.Helper()
	store := NewSessionMemoryStore(db)
	if err := store.Upsert(&MemorySession{
		SessionID:  sessionID,
		UserID:     userID,
		ProjectDir: "/test",
		FilePath:   "/test/x.jsonl",
		FileMtime:  1.0,
		IndexedAt:  1.0,
		LastSeenAt: 1.0,
		Platform:   "claude-code",
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func newMessageTestDB(t *testing.T) (*DB, *MessageStore) {
	t.Helper()
	db, err := Open(":memory:", migrationsmemory.FS)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewMessageStore(db)
}

func TestMessageStore_InsertAndSearch(t *testing.T) {
	db, messages := newMessageTestDB(t)
	seedTestSession(t, db, "sess-1", "ian")

	msgs := []*Message{
		{SessionID: "sess-1", Role: "user", Timestamp: "2026-04-26T12:00:00Z",
			Content: "How do I configure FTS5 in arc-relay?"},
		{SessionID: "sess-1", Role: "assistant", Timestamp: "2026-04-26T12:00:01Z",
			Content: "FTS5 is enabled via the unicode61 tokenizer."},
		{SessionID: "sess-1", Role: "user", Timestamp: "2026-04-26T12:00:02Z",
			Content: "Unrelated question about deploys."},
	}
	if _, err := messages.BulkInsert(msgs); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	for _, m := range msgs {
		if m.ID == 0 {
			t.Fatalf("BulkInsert did not populate ID for %+v", m)
		}
	}

	hits, err := messages.Search("ian", "FTS5", SearchOpts{Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 hits, got %d", len(hits))
	}

	// Verify user scoping: search as a different user returns nothing.
	otherHits, err := messages.Search("not-ian", "FTS5", SearchOpts{Limit: 10})
	if err != nil {
		t.Fatalf("scoped search: %v", err)
	}
	if len(otherHits) != 0 {
		t.Fatalf("user scoping leaked: %d hits for not-ian", len(otherHits))
	}
}

func TestMessageStore_RegexFallback(t *testing.T) {
	db, messages := newMessageTestDB(t)
	seedTestSession(t, db, "s", "ian")
	_, _ = messages.BulkInsert([]*Message{
		{SessionID: "s", Role: "user", Timestamp: "t1", Content: "deploy to staging"},
		{SessionID: "s", Role: "user", Timestamp: "t2", Content: "deploy to prod"},
		{SessionID: "s", Role: "user", Timestamp: "t3", Content: "no match here"},
	})

	hits, err := messages.SearchRegex("ian", `deploy.*(staging|prod)`, SearchOpts{Limit: 10})
	if err != nil {
		t.Fatalf("regex search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 regex hits, got %d", len(hits))
	}
}

func TestMessageStore_GetSession(t *testing.T) {
	db, messages := newMessageTestDB(t)
	seedTestSession(t, db, "s", "ian")
	_, _ = messages.BulkInsert([]*Message{
		{SessionID: "s", Role: "user", Timestamp: "t1", Epoch: 0, Content: "a"},
		{SessionID: "s", Role: "user", Timestamp: "t2", Epoch: 1, Content: "b"},
	})

	rows, err := messages.GetSession("s", 1)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(rows) != 1 || rows[0].Content != "b" {
		t.Fatalf("epoch filter broken: %+v", rows)
	}
}

func TestMessageStore_SearchProjectFilter(t *testing.T) {
	db, messages := newMessageTestDB(t)
	// Two sessions in different projects, same user
	seedTestSession(t, db, "s1", "ian")
	seedTestSession(t, db, "s2", "ian")
	// Override s2's project_dir to be different
	_, _ = db.Exec(`UPDATE memory_sessions SET project_dir = ? WHERE session_id = ?`, "/other-project", "s2")

	_, _ = messages.BulkInsert([]*Message{
		{SessionID: "s1", Role: "user", Timestamp: "t", Content: "FTS5 query"},
		{SessionID: "s2", Role: "user", Timestamp: "t", Content: "FTS5 query"},
	})

	hits, err := messages.Search("ian", "FTS5", SearchOpts{ProjectDir: "/other-project", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].SessionID != "s2" {
		t.Fatalf("project filter broken: %+v", hits)
	}
}
