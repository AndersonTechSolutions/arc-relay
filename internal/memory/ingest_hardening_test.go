package memory

import (
	"errors"
	"testing"
	"time"

	"github.com/comma-compliance/arc-relay/internal/store"
)

func chunk(sessionID string, lines ...[2]string) []byte {
	out := ""
	for _, l := range lines {
		out += `{"type":"user","uuid":"` + l[0] + `","timestamp":"` + l[1] + `","message":{"role":"user","content":"hello ` + l[0] + `"}}` + "\n"
	}
	return []byte(out)
}

func ingestReq(sessionID string, jsonl []byte) *IngestRequest {
	return &IngestRequest{
		SessionID: sessionID, ProjectDir: "/p", FilePath: "/f", FileMtime: 1,
		BytesSeen: int64(len(jsonl)), Platform: "claude-code", JSONL: jsonl,
	}
}

func TestIngest_ReplayIsIdempotent(t *testing.T) {
	svc, sessions := newServiceTestRig(t)
	body := chunk("s1", [2]string{"a", "2026-09-17T04:00:00.000Z"}, [2]string{"b", "2026-09-17T04:00:01.000Z"})

	first, err := svc.Ingest("ian", ingestReq("s1", body))
	if err != nil || first.MessagesAdded != 2 {
		t.Fatalf("first ingest: added=%d err=%v, want 2", first.MessagesAdded, err)
	}
	again, err := svc.Ingest("ian", ingestReq("s1", body))
	if err != nil || again.MessagesAdded != 0 {
		t.Fatalf("replay: added=%d err=%v, want 0", again.MessagesAdded, err)
	}
	if n, _ := sessions.CountMessages("s1"); n != 2 {
		t.Errorf("stored rows = %d, want 2", n)
	}
}

func TestIngest_ForeignSessionIs409AndWritesNothing(t *testing.T) {
	svc, sessions := newServiceTestRig(t)
	if _, err := svc.Ingest("ian", ingestReq("s1", chunk("s1", [2]string{"a", "2026-09-17T04:00:00Z"}))); err != nil {
		t.Fatal(err)
	}

	_, err := svc.Ingest("mallory", ingestReq("s1", chunk("s1", [2]string{"evil", "2026-09-17T09:00:00Z"})))
	if !errors.Is(err, store.ErrForeignSession) {
		t.Fatalf("foreign ingest err = %v, want ErrForeignSession", err)
	}
	if n, _ := sessions.CountMessages("s1"); n != 1 {
		t.Errorf("rows after rejected ingest = %d, want 1", n)
	}
	sess, err := sessions.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.UserID != "ian" {
		t.Errorf("owner rewritten to %q", sess.UserID)
	}
	var activity float64
	if err := svc.messages.DB().QueryRow(`SELECT last_activity_at FROM memory_sessions WHERE session_id='s1'`).Scan(&activity); err != nil {
		t.Fatal(err)
	}
	if activity != 1789617600 { // 2026-09-17T04:00:00Z — the rejected 09:00 message must not advance it
		t.Errorf("last_activity_at = %v, want 1789617600", activity)
	}

	owned, err := svc.SessionOwnedBy("ian", "s1")
	if err != nil || !owned {
		t.Errorf("SessionOwnedBy(ian) = %v, %v; want true", owned, err)
	}
	owned, _ = svc.SessionOwnedBy("mallory", "s1")
	if owned {
		t.Error("SessionOwnedBy(mallory) = true, want false")
	}
	owned, _ = svc.SessionOwnedBy("ian", "missing")
	if owned {
		t.Error("SessionOwnedBy(missing) = true, want false")
	}
}

func TestIngest_StampsActivityIngestedAtAndCounters(t *testing.T) {
	svc, _ := newServiceTestRig(t)
	fixed := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return fixed }

	req := ingestReq("s1", chunk("s1",
		[2]string{"a", "2026-09-17T04:00:00.500Z"},
		[2]string{"b", "garbage"},
		[2]string{"c", "2026-09-16T10:00:00+02:00"}, // older, must not win
	))
	req.ClientCounters = map[string]int64{"skipped_records": 3}
	req.RawCwd = "/home/dev/projects/arc-relay/.claude/worktrees/x"
	req.ProjectKey = "arc-relay"
	if _, err := svc.Ingest("ian", req); err != nil {
		t.Fatal(err)
	}

	var activity, ingested float64
	var counters, rawCwd, projectKey string
	if err := svc.messages.DB().QueryRow(`
		SELECT last_activity_at, last_ingested_at, client_counters, raw_cwd, project_key
		FROM memory_sessions WHERE session_id='s1'`).Scan(&activity, &ingested, &counters, &rawCwd, &projectKey); err != nil {
		t.Fatal(err)
	}
	if activity != 1789617600.5 {
		t.Errorf("last_activity_at = %v, want 1789617600.5 (newest parseable, offset honoured)", activity)
	}
	if ingested != float64(fixed.Unix()) {
		t.Errorf("last_ingested_at = %v, want %v", ingested, fixed.Unix())
	}
	if counters != `{"skipped_records":3}` {
		t.Errorf("client_counters = %q", counters)
	}
	if rawCwd == "" || projectKey != "arc-relay" {
		t.Errorf("source identity not stored: raw_cwd=%q project_key=%q", rawCwd, projectKey)
	}

	// A later chunk with only unparseable timestamps leaves activity alone,
	// and one without counters keeps the previous blob. Identity is insert-only.
	req2 := ingestReq("s1", chunk("s1", [2]string{"d", "nope"}))
	req2.ProjectKey = "something-else"
	if _, err := svc.Ingest("ian", req2); err != nil {
		t.Fatal(err)
	}
	if err := svc.messages.DB().QueryRow(`
		SELECT last_activity_at, client_counters, project_key FROM memory_sessions WHERE session_id='s1'`).Scan(&activity, &counters, &projectKey); err != nil {
		t.Fatal(err)
	}
	if activity != 1789617600.5 || counters != `{"skipped_records":3}` || projectKey != "arc-relay" {
		t.Errorf("second ingest changed insert-only/unchanged fields: activity=%v counters=%q project_key=%q", activity, counters, projectKey)
	}
}

func TestParseTimestamp(t *testing.T) {
	cases := map[string]bool{
		"2026-09-17T04:31:02.123Z":  true,
		"2026-09-17T04:31:02Z":      true,
		"2026-09-16T10:00:00+02:00": true,
		"2026-09-16 10:00:00":       true,
		"t":                         false,
		"":                          false,
	}
	for in, want := range cases {
		if _, ok := parseTimestamp(in); ok != want {
			t.Errorf("parseTimestamp(%q) ok=%v, want %v", in, ok, want)
		}
	}
}
