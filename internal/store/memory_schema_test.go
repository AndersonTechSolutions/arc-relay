package store

import (
	"testing"

	migrationsmemory "github.com/comma-compliance/arc-relay/migrations-memory"
)

func TestMemorySchema(t *testing.T) {
	db, err := Open(":memory:", migrationsmemory.FS)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	wantTables := []string{
		"memory_sessions",
		"memory_messages",
		"memory_compact_events",
	}
	for _, name := range wantTables {
		var got string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name,
		).Scan(&got)
		if err != nil {
			t.Fatalf("missing table %q: %v", name, err)
		}
	}

	// FTS5 virtual table
	var fts string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='memory_messages_fts'`,
	).Scan(&fts); err != nil {
		t.Fatalf("missing memory_messages_fts: %v", err)
	}

	// Triggers
	wantTriggers := []string{
		"memory_messages_ai",
		"memory_messages_ad",
		"memory_messages_au",
	}
	for _, name := range wantTriggers {
		var got string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='trigger' AND name=?`, name,
		).Scan(&got)
		if err != nil {
			t.Fatalf("missing trigger %q: %v", name, err)
		}
	}
}
