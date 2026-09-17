package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newWatcher(t *testing.T, srv *httptest.Server, dir string) *MemoryWatcher {
	t.Helper()
	return &MemoryWatcher{
		BaseURL: srv.URL, APIKey: "test", RootDir: filepath.Join(dir, "claude-projects"),
		StatePath: filepath.Join(dir, "state.json"), HTTPClient: srv.Client(),
	}
}

// A v1 state file is imported once into the v2 file with its watermarks
// intact (no replay), and is never written again so a downgrade still has
// its own last-known state.
func TestMemoryWatcher_ImportsV1StateWithoutReplay(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude-projects")
	path, want := writeTranscript(t, root, 5)
	v1 := stateFileV1{Files: map[string]*fileState{path: {BytesSeen: int64(len(want)), Mtime: 1}}}
	b, _ := json.Marshal(v1)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	srv, got := newIngestServer(t, nil)
	defer srv.Close()
	w := newWatcher(t, srv, dir)
	if err := w.RunOnce(); err != nil {
		t.Fatalf("run once: %v", err)
	}
	if n := len(got()); n != 0 {
		t.Fatalf("imported watermark was ignored: %d POSTs, want 0", n)
	}
	if wm := readWatermark(t, w.StatePath, path); wm != int64(len(want)) {
		t.Fatalf("v2 watermark = %d, want %d", wm, len(want))
	}
	after, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if string(after) != string(b) {
		t.Fatal("legacy v1 state was rewritten")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.v2.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("atomic save left its temp file behind")
	}
}

func TestStateLock_SecondWatcherIsRefused(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "state.lock")
	first, err := acquireStateLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireStateLock(lock); err == nil {
		t.Fatal("second acquire succeeded while the first is held")
	}
	first.release()
	second, err := acquireStateLock(lock)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	second.release()

	// The watcher itself takes the lock, so a concurrent --once is refused.
	srv, _ := newIngestServer(t, nil)
	defer srv.Close()
	w := newWatcher(t, srv, dir)
	held, err := acquireStateLock(w.lockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()
	if err := w.RunOnce(); err == nil || !strings.Contains(err.Error(), "another arc-sync") {
		t.Fatalf("RunOnce under a held lock: err = %v", err)
	}
}

// Directories created after the watcher started (Claude makes one per
// project) must be watched, or their transcripts are only seen on the 30 s
// tick. This drives the real loop, not RunOnce.
func TestMemoryWatcher_WatchesDirectoriesCreatedLater(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude-projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	srv, got := newIngestServer(t, nil)
	defer srv.Close()
	w := newWatcher(t, srv, dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.RunContext(ctx) }()
	time.Sleep(200 * time.Millisecond) // let the initial scan and fsnotify setup finish

	newProject := filepath.Join(root, "-Users-ian-later")
	if err := os.MkdirAll(newProject, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	line := `{"type":"user","uuid":"late","timestamp":"t","cwd":"/Users/ian/later","message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(filepath.Join(newProject, "new.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if reqs := got(); len(reqs) == 1 {
			if reqs[0].SessionID != "new" || reqs[0].RawCwd != "/Users/ian/later" {
				t.Fatalf("unexpected request: %+v", reqs[0])
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("RunContext: %v", err)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("transcript in a directory created after startup was never ingested (30 s tick would be the only fallback)")
}

// A line over the record ceiling is skipped without being buffered, counted,
// and the rest of the file still flows.
func TestMemoryWatcher_SkipsRecordOverCeilingAndCounts(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude-projects")
	projectDir := filepath.Join(root, "-Users-ian")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	huge := `{"type":"user","uuid":"big","timestamp":"t","message":{"role":"user","content":"` + strings.Repeat("x", 600) + `"}}` + "\n"
	small := `{"type":"user","uuid":"small","timestamp":"t","message":{"role":"user","content":"ok"}}` + "\n"
	path := filepath.Join(projectDir, "abc.jsonl")
	if err := os.WriteFile(path, []byte(small+huge+small), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, got := newIngestServer(t, nil)
	defer srv.Close()
	w := newWatcher(t, srv, dir)
	w.MaxChunkBytes = 256 // the huge line does not fit a window
	w.RecordCeiling = 400 // ...and is over the ceiling
	if err := w.RunOnce(); err != nil {
		t.Fatalf("run once: %v", err)
	}

	var delivered []byte
	for _, r := range got() {
		delivered = append(delivered, r.JSONL...)
	}
	if string(delivered) != small+small {
		t.Errorf("delivered %q, want the two small records only", delivered)
	}
	if wm := readWatermark(t, w.StatePath, path); wm != int64(len(small+huge+small)) {
		t.Errorf("watermark = %d, want EOF %d", wm, len(small+huge+small))
	}
	fs := readFileState(t, w.StatePath, path)
	if fs == nil || fs.Counters["skipped_records"] != 1 {
		t.Errorf("skipped_records counter = %v, want 1", fs)
	}
	reqs := got()
	if last := reqs[len(reqs)-1]; last.ClientCounters["skipped_records"] != 1 {
		t.Errorf("last request client_counters = %v, want skipped_records:1", last.ClientCounters)
	}
}

// A line within the ceiling but larger than a window is sent on its own.
func TestMemoryWatcher_SendsLineLargerThanWindowWhole(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude-projects")
	projectDir := filepath.Join(root, "-Users-ian")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	long := `{"type":"user","uuid":"long","timestamp":"t","message":{"role":"user","content":"` + strings.Repeat("y", 500) + `"}}` + "\n"
	path := filepath.Join(projectDir, "abc.jsonl")
	if err := os.WriteFile(path, []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, got := newIngestServer(t, nil)
	defer srv.Close()
	w := newWatcher(t, srv, dir)
	w.MaxChunkBytes = 256
	if err := w.RunOnce(); err != nil {
		t.Fatalf("run once: %v", err)
	}
	reqs := got()
	if len(reqs) != 1 || string(reqs[0].JSONL) != long {
		t.Fatalf("want the long line delivered whole in 1 POST, got %d POSTs", len(reqs))
	}
	if wm := readWatermark(t, w.StatePath, path); wm != int64(len(long)) {
		t.Errorf("watermark = %d, want %d", wm, len(long))
	}
}

// 413 on a multi-record unit is retried as halves; only a single record that
// is still too large is skipped and counted.
func TestMemoryWatcher_HalvesOn413ThenSkipsSingleRecord(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude-projects")
	path, want := writeTranscript(t, root, 8)

	// Reject every unit with more than one line (a proxy limit below our
	// chunk size), and the single record with uuid u3 (over any limit).
	var delivered []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ingestRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		body := string(req.JSONL)
		if strings.Count(body, "\n") > 1 || strings.Contains(body, `"uuid":"u3"`) {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		delivered = append(delivered, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"messages_added": 1})
	}))
	defer srv.Close()

	w := newWatcher(t, srv, dir)
	if err := w.RunOnce(); err != nil {
		t.Fatalf("run once: %v", err)
	}
	joined := strings.Join(delivered, "")
	for i := 0; i < 8; i++ {
		tag := fmt.Sprintf(`"uuid":"u%d"`, i)
		if i == 3 {
			if strings.Contains(joined, tag) {
				t.Errorf("u3 was delivered although the relay rejects it")
			}
			continue
		}
		if !strings.Contains(joined, tag) {
			t.Errorf("record %s missing after halving", tag)
		}
	}
	if wm := readWatermark(t, w.StatePath, path); wm != int64(len(want)) {
		t.Errorf("watermark = %d, want EOF %d", wm, len(want))
	}
	if fs := readFileState(t, w.StatePath, path); fs == nil || fs.Counters["skipped_records"] != 1 {
		t.Errorf("skipped_records = %v, want 1", fs)
	}
}

// 409 means the session belongs to another relay user: the file is
// quarantined, the watermark stays, and later scans do not touch it.
func TestMemoryWatcher_QuarantinesOn409(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude-projects")
	path, _ := writeTranscript(t, root, 3)
	srv, got := newIngestServer(t, func(int) int { return http.StatusConflict })
	defer srv.Close()
	w := newWatcher(t, srv, dir)
	if err := w.RunOnce(); err != nil {
		t.Fatalf("run once: %v", err)
	}
	if err := w.RunOnce(); err != nil {
		t.Fatalf("second run once: %v", err)
	}
	if n := len(got()); n != 1 {
		t.Fatalf("POSTs = %d, want exactly 1 (quarantined file must not be retried)", n)
	}
	fs := readFileState(t, w.StatePath, path)
	if fs == nil || fs.Quarantined != "ownership_conflict" || fs.BytesSeen != 0 || fs.Counters["ownership_conflicts"] != 1 {
		t.Fatalf("file state after 409 = %+v", fs)
	}
}

func TestMemoryWatcher_SendsRawCwdFromTranscriptHead(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude-projects")
	projectDir := filepath.Join(root, "-Users-ian-my-app")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := `{"type":"summary","summary":"x"}` + "\n" +
		`{"type":"user","uuid":"u1","timestamp":"t","cwd":"/Users/ian/my-app","message":{"role":"user","content":"hi"}}` + "\n"
	path := filepath.Join(projectDir, "abc.jsonl")
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, got := newIngestServer(t, nil)
	defer srv.Close()
	w := newWatcher(t, srv, dir)
	if err := w.RunOnce(); err != nil {
		t.Fatalf("run once: %v", err)
	}
	reqs := got()
	if len(reqs) != 1 || reqs[0].RawCwd != "/Users/ian/my-app" {
		t.Fatalf("raw_cwd not sent: %+v", reqs)
	}
	if reqs[0].ProjectDir != "/Users/ian/my/app" {
		t.Errorf("project_dir = %q (lossy decode is still the informational value)", reqs[0].ProjectDir)
	}
}

func TestPostExtractMode_ManualAndReason(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": gotBody["session_id"], "mode": gotBody["mode"], "queued": false, "reason": "ineligible:subagent",
		})
	}))
	defer srv.Close()
	w := &MemoryWatcher{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()}
	res, err := w.PostExtractMode("s1", ExtractModeManual)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["mode"] != "manual" || res.Queued || res.Reason != "ineligible:subagent" {
		t.Fatalf("body=%v res=%+v", gotBody, res)
	}

	// A pre-Phase-0 relay answers without queued/reason: treated as queued.
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"accepted"}`))
	}))
	defer legacy.Close()
	w.BaseURL = legacy.URL
	w.HTTPClient = legacy.Client()
	res, err = w.PostExtractMode("s1", ExtractModeAuto)
	if err != nil || !res.Queued {
		t.Fatalf("legacy relay: res=%+v err=%v", res, err)
	}
}

func TestHalveAtNewline(t *testing.T) {
	a, b := halveAtNewline([]byte("aa\nbb\ncc\ndd\n"))
	if string(a) != "aa\nbb\n" || string(b) != "cc\ndd\n" {
		t.Errorf("halves = %q / %q", a, b)
	}
	a, b = halveAtNewline([]byte("a\n" + strings.Repeat("x", 50) + "\n"))
	if string(a) != "a\n" || !strings.HasPrefix(string(b), "xxx") {
		t.Errorf("uneven halves = %q / %q", a, b)
	}
	a, b = halveAtNewline([]byte("single\n"))
	if string(a) != "single\n" || b != nil {
		t.Errorf("single line = %q / %q", a, b)
	}
}
