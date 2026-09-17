package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/comma-compliance/arc-relay/internal/mcp"
	"github.com/comma-compliance/arc-relay/internal/store"
	migrationsmemory "github.com/comma-compliance/arc-relay/migrations-memory"
)

// fakeBackend answers add_memory with one memory id per call and can be
// told to fail specific call numbers (1-based) or every call.
type fakeBackend struct {
	mu      sync.Mutex
	calls   int
	failOn  map[int]bool
	failAll bool
}

func (f *fakeBackend) Send(ctx context.Context, req *mcp.Request) (*mcp.Response, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if f.failAll || f.failOn[n] {
		return nil, errors.New("mem0 boom")
	}
	text, _ := json.Marshal(fmt.Sprintf(`{"results":[{"id":"m%d"}]}`, n))
	return &mcp.Response{Result: json.RawMessage(`{"content":[{"type":"text","text":` + string(text) + `}]}`)}, nil
}

func (f *fakeBackend) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type rig struct {
	svc      *Service
	db       *store.DB
	sessions *store.SessionMemoryStore
	messages *store.MessageStore
	backend  *fakeBackend
	clock    time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	// A file-backed DB, not ":memory:": the queue's workers run on their own
	// pool connections, and every new connection to ":memory:" is a separate
	// empty database.
	db, err := store.Open(filepath.Join(t.TempDir(), "memory.db"), migrationsmemory.FS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sessions := store.NewSessionMemoryStore(db)
	messages := store.NewMessageStore(db)
	backend := &fakeBackend{failOn: map[int]bool{}}
	svc := NewService(sessions, messages, store.NewExtractionStore(db),
		func() (Backend, bool) { return backend, true }, nil)
	r := &rig{svc: svc, db: db, sessions: sessions, messages: messages, backend: backend,
		clock: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)}
	svc.now = func() time.Time { return r.clock }
	// One message per chunk, two messages per pass, so pass arithmetic is
	// easy to read in the assertions.
	svc.chunkTarget = 250
	svc.passLimit = 2
	return r
}

const prose = "We decided to move the relay's memory watcher to a bounded pass model because the old one could livelock. " +
	"The decision was recorded in the spec and the migration was measured on a copy of production first."

func (r *rig) seedSession(t *testing.T, id, platform string, msgs int, role string) {
	t.Helper()
	if err := r.sessions.Upsert(&store.MemorySession{
		SessionID: id, UserID: "u1", ProjectDir: "/home/dev/projects/arc-relay", FilePath: "/f",
		FileMtime: 1, IndexedAt: 1, LastSeenAt: 1, Platform: platform,
	}); err != nil {
		t.Fatal(err)
	}
	var batch []*store.Message
	for i := 1; i <= msgs; i++ {
		batch = append(batch, &store.Message{
			UUID: fmt.Sprintf("%s-u%d", id, i), SessionID: id, Timestamp: "2026-09-17T11:00:00Z",
			Role: role, Content: fmt.Sprintf("%d: %s", i, prose), Platform: platform,
		})
	}
	if _, err := r.messages.BulkInsert(batch); err != nil {
		t.Fatal(err)
	}
	r.exec(t, `UPDATE memory_sessions SET last_activity_at = ?, last_ingested_at = ? WHERE session_id = ?`,
		float64(r.clock.Unix())-3600, float64(r.clock.Unix())-3600, id)
}

func (r *rig) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := r.db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (r *rig) state(t *testing.T, id string) *store.ExtractionState {
	t.Helper()
	st, err := r.sessions.GetExtractionState(id)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (r *rig) extract(t *testing.T, id string) *ExtractResult {
	t.Helper()
	res, err := r.svc.Extract(context.Background(), id)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return res
}

func TestExtract_BoundedPassWalksForward(t *testing.T) {
	r := newRig(t)
	r.seedSession(t, "s1", "claude-code", 5, "user")

	res := r.extract(t, "s1")
	if res.Outcome != OutcomeComplete || res.ThroughID != 2 || r.backend.count() != 2 {
		t.Fatalf("pass 1: outcome=%s through=%d calls=%d, want complete/2/2", res.Outcome, res.ThroughID, r.backend.count())
	}
	if st := r.state(t, "s1"); !st.ExtractedThroughMsgID.Valid || st.ExtractedThroughMsgID.Int64 != 2 || st.ExtractAttempts != 0 {
		t.Fatalf("pass 1 state: %+v", st)
	}
	res = r.extract(t, "s1")
	if res.Outcome != OutcomeComplete || res.ThroughID != 4 {
		t.Fatalf("pass 2: outcome=%s through=%d", res.Outcome, res.ThroughID)
	}
	res = r.extract(t, "s1")
	if res.Outcome != OutcomeComplete || res.ThroughID != 5 || r.backend.count() != 5 {
		t.Fatalf("pass 3: outcome=%s through=%d calls=%d", res.Outcome, res.ThroughID, r.backend.count())
	}
	res = r.extract(t, "s1")
	if res.Outcome != OutcomeNothingNew || r.backend.count() != 5 {
		t.Fatalf("pass 4: outcome=%s calls=%d, want nothing_new/5", res.Outcome, r.backend.count())
	}
}

func TestExtract_ToolOnlyWindowAdvancesWithoutCalls(t *testing.T) {
	r := newRig(t)
	r.seedSession(t, "s1", "claude-code", 2, "tool")

	res := r.extract(t, "s1")
	if res.Outcome != OutcomeAdvancedNoContent || res.ThroughID != 2 || r.backend.count() != 0 {
		t.Fatalf("outcome=%s through=%d calls=%d", res.Outcome, res.ThroughID, r.backend.count())
	}
	if st := r.state(t, "s1"); st.ExtractedThroughMsgID.Int64 != 2 {
		t.Fatalf("watermark not advanced: %+v", st)
	}
}

func TestExtract_PartialFailureRetriesWithoutReSpend(t *testing.T) {
	r := newRig(t)
	r.seedSession(t, "s1", "claude-code", 2, "user")
	r.backend.failOn[2] = true // first pass: chunk 1 ok, chunk 2 fails (no retry: "boom" is not retryable)

	res := r.extract(t, "s1")
	if res.Outcome != OutcomeFailed || len(res.Errors) != 1 {
		t.Fatalf("pass 1: outcome=%s errors=%v", res.Outcome, res.Errors)
	}
	st := r.state(t, "s1")
	if st.ExtractedThroughMsgID.Valid || st.ExtractAttempts != 1 || !st.LastExtractAttemptAt.Valid {
		t.Fatalf("after failure: %+v", st)
	}

	res = r.extract(t, "s1")
	if res.Outcome != OutcomeComplete || res.MessagesNew != 1 || r.backend.count() != 3 {
		t.Fatalf("retry: outcome=%s new=%d calls=%d, want complete/1/3 (succeeded chunk not re-sent)",
			res.Outcome, res.MessagesNew, r.backend.count())
	}
	if st := r.state(t, "s1"); st.ExtractedThroughMsgID.Int64 != 2 || st.ExtractAttempts != 0 {
		t.Fatalf("after retry: %+v", st)
	}
}

func TestExtract_PoisonAfterFourFailures(t *testing.T) {
	r := newRig(t)
	r.seedSession(t, "s1", "claude-code", 3, "user")
	r.backend.failAll = true

	for i := 1; i <= 3; i++ {
		if res := r.extract(t, "s1"); res.Outcome != OutcomeFailed {
			t.Fatalf("attempt %d outcome=%s", i, res.Outcome)
		}
	}
	res := r.extract(t, "s1")
	if res.Outcome != OutcomePoisoned || res.ThroughID != 2 {
		t.Fatalf("attempt 4: outcome=%s through=%d, want poisoned/2", res.Outcome, res.ThroughID)
	}
	st := r.state(t, "s1")
	if st.ExtractedThroughMsgID.Int64 != 2 || !st.ExtractPoisonedAt.Valid || st.ExtractAttempts != 0 {
		t.Fatalf("after poison: %+v", st)
	}
	// The rest of the session is still reachable.
	if res := r.extract(t, "s1"); res.Outcome != OutcomeFailed || res.ThroughID != 3 {
		t.Fatalf("after poison, next window: outcome=%s through=%d", res.Outcome, res.ThroughID)
	}
	n, err := r.sessions.CountPoisoned()
	if err != nil || n != 1 {
		t.Fatalf("CountPoisoned = %d, %v", n, err)
	}
}

func TestExtract_CancelledPassAbortsWithoutRows(t *testing.T) {
	r := newRig(t)
	r.seedSession(t, "s1", "claude-code", 2, "user")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := r.svc.Extract(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeFailed || r.backend.count() != 0 {
		t.Fatalf("outcome=%s calls=%d, want failed/0", res.Outcome, r.backend.count())
	}
	var rows int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM memory_extractions`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("provenance rows after abort = %d (%v), want 0", rows, err)
	}
	if st := r.state(t, "s1"); st.ExtractAttempts != 1 || st.ExtractedThroughMsgID.Valid {
		t.Fatalf("state after abort: %+v", st)
	}
}

func TestExtract_NullWatermarkCoveredSessionMakesNoCalls(t *testing.T) {
	r := newRig(t)
	r.seedSession(t, "s1", "claude-code", 2, "user")
	// Pre-migration history: a successful provenance row covers both uuids
	// but the watermark is NULL.
	r.exec(t, `INSERT INTO memory_extractions (session_id, extracted_at, chunk_index, chunk_msg_uuids, chunk_chars)
		VALUES ('s1', 1, 0, '["s1-u1","s1-u2"]', 10)`)

	res := r.extract(t, "s1")
	if res.Outcome != OutcomeAdvancedNoContent || r.backend.count() != 0 || res.ThroughID != 2 {
		t.Fatalf("outcome=%s calls=%d through=%d", res.Outcome, r.backend.count(), res.ThroughID)
	}
}

func TestTruncateOversized(t *testing.T) {
	m := &store.Message{Content: strings.Repeat("é", 100)}
	truncateOversized([]*store.Message{m}, 51) // 51 bytes lands mid-rune
	if !strings.HasPrefix(m.Content, strings.Repeat("é", 25)) || !strings.Contains(m.Content, "[truncated 150 bytes]") {
		t.Errorf("truncated content = %q", m.Content)
	}
	short := &store.Message{Content: "ok"}
	truncateOversized([]*store.Message{short}, 51)
	if short.Content != "ok" {
		t.Error("short message modified")
	}
}

func TestEligibility(t *testing.T) {
	r := newRig(t)
	now := float64(r.clock.Unix())
	mk := func(id string) { r.seedSession(t, id, "claude-code", 1, "user") }
	mk("eligible")
	mk("subagent")
	r.exec(t, `UPDATE memory_sessions SET is_subagent = 1 WHERE session_id = 'subagent'`)
	r.seedSession(t, "codex", "codex", 1, "user")
	mk("old")
	r.exec(t, `UPDATE memory_sessions SET last_activity_at = ? WHERE session_id = 'old'`, now-40*86400)
	mk("noactivity")
	r.exec(t, `UPDATE memory_sessions SET last_activity_at = NULL WHERE session_id = 'noactivity'`)
	mk("busy")
	r.exec(t, `UPDATE memory_sessions SET last_ingested_at = ? WHERE session_id = 'busy'`, now-10)
	mk("backoff")
	r.exec(t, `UPDATE memory_sessions SET extract_attempts = 1, last_extract_attempt_at = ? WHERE session_id = 'backoff'`, now-60)
	mk("backoffdone")
	r.exec(t, `UPDATE memory_sessions SET extract_attempts = 1, last_extract_attempt_at = ? WHERE session_id = 'backoffdone'`, now-400)
	mk("done")
	r.exec(t, `UPDATE memory_sessions SET extracted_through_msg_id = (SELECT MAX(id) FROM memory_messages WHERE session_id='done') WHERE session_id = 'done'`)

	cfg := store.DefaultEligibilityConfig()
	got, err := r.sessions.ListEligibleForAutoExtraction(now, cfg, 50)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"eligible", "backoffdone"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("eligible = %v, want %v (never-attempted first, then least recently attempted)", got, want)
	}
	for id, want := range map[string]bool{"eligible": true, "subagent": false, "codex": false, "old": false,
		"noactivity": false, "busy": false, "backoff": false, "backoffdone": true, "done": false} {
		ok, err := r.sessions.IsEligibleForAutoExtraction(id, now, cfg)
		if err != nil || ok != want {
			t.Errorf("IsEligible(%s) = %v, %v; want %v", id, ok, err, want)
		}
	}
	// Opening the platform gate admits the codex session.
	cfg.Platforms = []string{"claude-code", "codex"}
	if ok, _ := r.sessions.IsEligibleForAutoExtraction("codex", now, cfg); !ok {
		t.Error("codex not eligible after platform gate opened")
	}
}

func TestQueue_AdmissionAndDedup(t *testing.T) {
	r := newRig(t)
	r.seedSession(t, "s1", "claude-code", 2, "user")
	r.seedSession(t, "sub", "claude-code", 1, "user")
	r.exec(t, `UPDATE memory_sessions SET is_subagent = 1 WHERE session_id = 'sub'`)
	r.seedSession(t, "old", "claude-code", 1, "user")
	r.exec(t, `UPDATE memory_sessions SET last_activity_at = ? WHERE session_id = 'old'`, float64(r.clock.Unix())-40*86400)

	if ok, reason := r.svc.Enqueue("s1", ModeAuto); ok || reason != "queue_not_running" {
		t.Fatalf("before StartQueue: %v %s", ok, reason)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.svc.StartQueue(ctx, DefaultQueueConfig())

	// Hold s1's per-session lock so the worker blocks inside Extract and the
	// session stays pending while admission is asserted deterministically.
	lockI, _ := r.svc.locks.LoadOrStore("s1", &sync.Mutex{})
	lock := lockI.(*sync.Mutex)
	lock.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			lock.Unlock()
		}
	}()

	if ok, reason := r.svc.Enqueue("s1", ModeAuto); !ok || reason != "eligible" {
		t.Fatalf("eligible enqueue: %v %s", ok, reason)
	}
	if ok, reason := r.svc.Enqueue("s1", ModeAuto); ok || reason != "duplicate" {
		t.Fatalf("duplicate enqueue: %v %s", ok, reason)
	}
	if ok, reason := r.svc.Enqueue("sub", ModeAuto); ok || reason != "ineligible:auto_gate" {
		t.Fatalf("subagent auto: %v %s", ok, reason)
	}
	if ok, reason := r.svc.Enqueue("sub", ModeManual); ok || reason != "ineligible:subagent" {
		t.Fatalf("subagent manual: %v %s", ok, reason)
	}
	if ok, reason := r.svc.Enqueue("old", ModeAuto); ok || reason != "ineligible:auto_gate" {
		t.Fatalf("old auto: %v %s", ok, reason)
	}
	if ok, reason := r.svc.Enqueue("old", ModeManual); !ok || reason != "eligible" {
		t.Fatalf("old manual: %v %s", ok, reason)
	}
	if ok, reason := r.svc.Enqueue("missing", ModeManual); ok || reason != "ineligible:unknown_session" {
		t.Fatalf("missing manual: %v %s", ok, reason)
	}

	// Release the lock: the worker's pass lands and s1 leaves pending.
	lock.Unlock()
	unlocked = true
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := r.state(t, "s1"); st.ExtractedThroughMsgID.Valid && st.ExtractedThroughMsgID.Int64 == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := r.state(t, "s1"); !st.ExtractedThroughMsgID.Valid {
		t.Fatalf("worker never extracted s1: %+v", st)
	}
	// Once finished, the session may be enqueued again (no longer pending),
	// but it is no longer eligible: nothing above the watermark.
	for time.Now().Before(deadline) {
		if _, reason := r.svc.Enqueue("s1", ModeAuto); reason != "duplicate" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ok, reason := r.svc.Enqueue("s1", ModeAuto); ok || reason != "ineligible:auto_gate" {
		t.Fatalf("re-enqueue after completion: %v %s", ok, reason)
	}
}

func TestRateLimiter_StartsEmptyAndRefills(t *testing.T) {
	l := NewRateLimiter(3600, 60) // 1 token/s
	clock := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return clock }

	if ok, wait := l.take(); ok || wait <= 0 {
		t.Fatalf("startup take = %v %v, want blocked", ok, wait)
	}
	clock = clock.Add(1500 * time.Millisecond)
	if ok, _ := l.take(); !ok {
		t.Fatal("no token after 1.5s at 1/s")
	}
	if ok, _ := l.take(); ok {
		t.Fatal("second token available too early")
	}
	clock = clock.Add(10 * time.Minute)
	for i := 0; i < 60; i++ {
		if ok, _ := l.take(); !ok {
			t.Fatalf("burst capped below 60 at %d", i)
		}
	}
	if ok, _ := l.take(); ok {
		t.Fatal("burst exceeded 60")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); err == nil {
		t.Fatal("Wait with no tokens and a short ctx should return ctx error")
	}
	var nilLimiter *RateLimiter
	if err := nilLimiter.Wait(context.Background()); err != nil {
		t.Fatal("nil limiter must not block")
	}
}
