package sync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestMemoryWatcher_RunOnceIngestsDelta(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "claude-projects", "-Users-ian")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(projectDir, "abc.jsonl")
	transcript := `{"type":"user","uuid":"u1","timestamp":"t","message":{"role":"user","content":"first"}}` + "\n"
	if err := os.WriteFile(jsonlPath, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		received [][]byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages_added": 1, "events_added": 0, "bytes_seen": int64(len(body)),
		})
	}))
	defer server.Close()

	w := &MemoryWatcher{
		BaseURL:    server.URL,
		APIKey:     "test",
		RootDir:    filepath.Join(dir, "claude-projects"),
		StatePath:  filepath.Join(dir, "state.json"),
		HTTPClient: server.Client(),
	}
	if err := w.RunOnce(); err != nil {
		t.Fatalf("run once: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("want 1 POST, got %d", len(received))
	}

	// Decode and verify the body shape
	var got ingestRequest
	if err := json.Unmarshal(received[0], &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.SessionID != "abc" {
		t.Fatalf("want session_id=abc, got %q", got.SessionID)
	}
	if got.Platform != "claude-code" {
		t.Fatalf("want platform=claude-code, got %q", got.Platform)
	}
	if string(got.JSONL) != transcript {
		t.Fatalf("jsonl body mismatch: %q", got.JSONL)
	}

	// Verify watermark file was written
	stateData, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var st stateFile
	if err := json.Unmarshal(stateData, &st); err != nil {
		t.Fatalf("parsing state: %v", err)
	}
	fs := st.Files[jsonlPath]
	if fs == nil || fs.BytesSeen == 0 {
		t.Fatalf("watermark not written after successful POST")
	}
}

func TestMemoryWatcher_NoOpOnNoChange(t *testing.T) {
	dir := t.TempDir()
	pd := filepath.Join(dir, "p", "-x")
	_ = os.MkdirAll(pd, 0o755)
	_ = os.WriteFile(filepath.Join(pd, "s1.jsonl"), []byte("{}\n"), 0o644)

	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages_added": 0, "events_added": 0, "bytes_seen": 3,
		})
	}))
	defer server.Close()

	w := &MemoryWatcher{
		BaseURL:    server.URL,
		APIKey:     "k",
		RootDir:    filepath.Join(dir, "p"),
		StatePath:  filepath.Join(dir, "state.json"),
		HTTPClient: server.Client(),
	}
	_ = w.RunOnce()
	_ = w.RunOnce() // second call should be a no-op — watermark already at file size
	if posts != 1 {
		t.Fatalf("want 1 POST, got %d (no-op detection broken)", posts)
	}
}

func TestMemoryWatcher_HTTPFailureDoesNotAdvanceWatermark(t *testing.T) {
	dir := t.TempDir()
	pd := filepath.Join(dir, "p", "-x")
	_ = os.MkdirAll(pd, 0o755)
	_ = os.WriteFile(filepath.Join(pd, "s1.jsonl"), []byte("{}\n"), 0o644)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	w := &MemoryWatcher{
		BaseURL:    server.URL,
		APIKey:     "k",
		RootDir:    filepath.Join(dir, "p"),
		StatePath:  filepath.Join(dir, "state.json"),
		HTTPClient: server.Client(),
	}
	_ = w.RunOnce()

	// Watermark should NOT have advanced
	st := w.loadState()
	for path, fs := range st.Files {
		if fs.BytesSeen != 0 {
			t.Fatalf("watermark advanced on failure: %s = %d", path, fs.BytesSeen)
		}
	}
}

func TestMemoryWatcher_CorruptStateFileResetsCleanly(t *testing.T) {
	dir := t.TempDir()
	pd := filepath.Join(dir, "p", "-x")
	_ = os.MkdirAll(pd, 0o755)
	_ = os.WriteFile(filepath.Join(pd, "s1.jsonl"), []byte("{}\n"), 0o644)

	statePath := filepath.Join(dir, "state.json")
	// Pre-write a corrupted state file
	_ = os.WriteFile(statePath, []byte("{not json"), 0o600)

	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages_added": 0, "events_added": 0, "bytes_seen": 3,
		})
	}))
	defer server.Close()

	w := &MemoryWatcher{
		BaseURL:    server.URL,
		APIKey:     "k",
		RootDir:    filepath.Join(dir, "p"),
		StatePath:  statePath,
		HTTPClient: server.Client(),
	}
	if err := w.RunOnce(); err != nil {
		t.Fatalf("run once: %v", err)
	}
	// We don't assert post count beyond "no crash" — the test's value is
	// proving the watcher recovers from corrupt state without panicking.
	if posts < 1 {
		t.Fatalf("expected at least 1 POST after recovery, got %d", posts)
	}
}

func TestMemoryWatcher_DecodesProjectDir(t *testing.T) {
	cases := []struct {
		escaped, want string
	}{
		{"-Users-ian", "/Users/ian"},
		{"-Users-ian-code-arc-relay", "/Users/ian/code/arc/relay"},
		{"-tmp-test", "/tmp/test"},
	}
	for _, c := range cases {
		got := decodeProjectDir(c.escaped)
		if got != c.want {
			t.Errorf("decodeProjectDir(%q) = %q, want %q", c.escaped, got, c.want)
		}
	}
}

// newIngestServer returns an httptest server that records every ingest body and
// answers with the status/response chosen by status(callIndex). A zero status
// means 200.
func newIngestServer(t *testing.T, status func(int) int) (*httptest.Server, func() []ingestRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []ingestRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got ingestRequest
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode ingest body: %v", err)
		}
		mu.Lock()
		n := len(reqs)
		reqs = append(reqs, got)
		mu.Unlock()

		if status != nil {
			if code := status(n); code != 0 {
				http.Error(w, "rejected", code)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages_added": 1, "events_added": 0, "bytes_seen": got.BytesSeen,
		})
	}))
	return srv, func() []ingestRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]ingestRequest(nil), reqs...)
	}
}

// writeTranscript writes n JSONL records and returns the path and raw bytes.
func writeTranscript(t *testing.T, root string, n int) (string, []byte) {
	t.Helper()
	projectDir := filepath.Join(root, "-Users-ian")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var buf []byte
	for i := 0; i < n; i++ {
		line := fmt.Sprintf(`{"type":"user","uuid":"u%d","timestamp":"t","message":{"role":"user","content":"%s"}}`, i, strings.Repeat("x", 64))
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	path := filepath.Join(projectDir, "abc.jsonl")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, buf
}

func readWatermark(t *testing.T, statePath, filePath string) int64 {
	t.Helper()
	b, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		// Nothing was ever committed — an unwritten state file means watermark 0.
		return 0
	}
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var st stateFile
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if fs := st.Files[filePath]; fs != nil {
		return fs.BytesSeen
	}
	return 0
}

// A delta larger than the chunk limit must be split across several POSTs, each
// one newline-aligned, together reproducing the file exactly. Before chunking
// this was a single oversized POST that the relay rejected forever.
func TestMemoryWatcher_ChunksLargeDelta(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude-projects")
	path, want := writeTranscript(t, root, 50)

	srv, got := newIngestServer(t, nil)
	defer srv.Close()

	w := &MemoryWatcher{
		BaseURL: srv.URL, APIKey: "test", RootDir: root,
		StatePath: filepath.Join(dir, "state.json"), HTTPClient: srv.Client(),
		MaxChunkBytes: 512,
	}
	if err := w.RunOnce(); err != nil {
		t.Fatalf("run once: %v", err)
	}

	reqs := got()
	if len(reqs) < 2 {
		t.Fatalf("want the delta split across multiple POSTs, got %d", len(reqs))
	}

	var reassembled []byte
	for i, r := range reqs {
		if len(r.JSONL) > 512 && bytes.Count(r.JSONL, []byte("\n")) > 1 {
			t.Errorf("chunk %d is %d bytes, over the 512 limit", i, len(r.JSONL))
		}
		if r.JSONL[len(r.JSONL)-1] != '\n' {
			t.Errorf("chunk %d does not end on a newline — a record was split", i)
		}
		reassembled = append(reassembled, r.JSONL...)
	}
	if !bytes.Equal(reassembled, want) {
		t.Errorf("reassembled chunks (%d bytes) != original transcript (%d bytes)", len(reassembled), len(want))
	}

	// Watermark must land exactly at EOF, and each chunk must report its own
	// cumulative offset rather than the whole file size.
	if wm := readWatermark(t, w.StatePath, path); wm != int64(len(want)) {
		t.Errorf("final watermark = %d, want %d", wm, len(want))
	}
	if reqs[0].BytesSeen >= int64(len(want)) {
		t.Errorf("first chunk reported bytes_seen=%d, want a partial offset", reqs[0].BytesSeen)
	}
}

// A 413 is permanent for those bytes, so the watcher must skip them and keep
// going. This is the regression guard for the infinite-retry bug.
func TestMemoryWatcher_SkipsChunkRejectedAsTooLarge(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude-projects")
	path, want := writeTranscript(t, root, 50)

	// Reject only the first chunk.
	srv, got := newIngestServer(t, func(n int) int {
		if n == 0 {
			return http.StatusRequestEntityTooLarge
		}
		return 0
	})
	defer srv.Close()

	w := &MemoryWatcher{
		BaseURL: srv.URL, APIKey: "test", RootDir: root,
		StatePath: filepath.Join(dir, "state.json"), HTTPClient: srv.Client(),
		MaxChunkBytes: 512,
	}
	if err := w.RunOnce(); err != nil {
		t.Fatalf("run once: %v", err)
	}

	reqs := got()
	if len(reqs) < 2 {
		t.Fatalf("watcher stopped after the 413 — got %d POSTs, want it to continue", len(reqs))
	}
	if wm := readWatermark(t, w.StatePath, path); wm != int64(len(want)) {
		t.Errorf("watermark = %d, want %d (must advance past the rejected chunk)", wm, len(want))
	}
}

// Anything that is not a 413 may succeed later, so the watermark must stay put
// and the same bytes must be retried on the next scan.
func TestMemoryWatcher_TransientErrorDoesNotAdvanceWatermark(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude-projects")
	path, _ := writeTranscript(t, root, 50)

	failing := true
	srv, got := newIngestServer(t, func(int) int {
		if failing {
			return http.StatusInternalServerError
		}
		return 0
	})
	defer srv.Close()

	w := &MemoryWatcher{
		BaseURL: srv.URL, APIKey: "test", RootDir: root,
		StatePath: filepath.Join(dir, "state.json"), HTTPClient: srv.Client(),
		MaxChunkBytes: 512,
	}
	if err := w.RunOnce(); err != nil {
		t.Fatalf("run once: %v", err)
	}
	if wm := readWatermark(t, w.StatePath, path); wm != 0 {
		t.Fatalf("watermark advanced to %d despite a 500; those bytes would be lost", wm)
	}

	// Once the relay recovers, the retry must deliver everything.
	failing = false
	if err := w.RunOnce(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	var delivered []byte
	for _, r := range got() {
		if r.JSONL != nil {
			delivered = append(delivered, r.JSONL...)
		}
	}
	if !bytes.Contains(delivered, []byte(`"uuid":"u49"`)) {
		t.Error("retry did not deliver the full transcript")
	}
}

// A transcript caught mid-append ends partway through a record. That fragment
// must be held back: the parser skips malformed lines, so sending it would drop
// the record on both sides of the split.
func TestMemoryWatcher_HoldsBackIncompleteTrailingLine(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude-projects")
	projectDir := filepath.Join(root, "-Users-ian")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	complete := `{"type":"user","uuid":"u1","timestamp":"t","message":{"role":"user","content":"done"}}` + "\n"
	partial := `{"type":"user","uuid":"u2","timestamp":"t","messa`
	path := filepath.Join(projectDir, "abc.jsonl")
	if err := os.WriteFile(path, []byte(complete+partial), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, got := newIngestServer(t, nil)
	defer srv.Close()

	w := &MemoryWatcher{
		BaseURL: srv.URL, APIKey: "test", RootDir: root,
		StatePath: filepath.Join(dir, "state.json"), HTTPClient: srv.Client(),
	}
	if err := w.RunOnce(); err != nil {
		t.Fatalf("run once: %v", err)
	}

	reqs := got()
	if len(reqs) != 1 {
		t.Fatalf("want 1 POST, got %d", len(reqs))
	}
	if string(reqs[0].JSONL) != complete {
		t.Errorf("sent %q, want only the complete line %q", reqs[0].JSONL, complete)
	}
	if wm := readWatermark(t, w.StatePath, path); wm != int64(len(complete)) {
		t.Errorf("watermark = %d, want %d (must stop at the last newline)", wm, len(complete))
	}
}

func TestChunkEnd(t *testing.T) {
	b := []byte("aaa\nbbb\nccc\n")
	cases := []struct {
		name     string
		off, max int
		want     int
	}{
		{"whole delta fits", 0, 100, 12},
		{"splits on newline boundary", 0, 6, 4},
		{"exact boundary", 0, 8, 8},
		{"resumes from offset", 4, 6, 8},
		{"last chunk", 8, 6, 12},
	}
	for _, c := range cases {
		if got := chunkEnd(b, c.off, c.max); got != c.want {
			t.Errorf("%s: chunkEnd(off=%d,max=%d) = %d, want %d", c.name, c.off, c.max, got, c.want)
		}
	}

	// A single line longer than max is emitted whole rather than cut in half.
	long := []byte(strings.Repeat("x", 50) + "\n" + "short\n")
	if got := chunkEnd(long, 0, 10); got != 51 {
		t.Errorf("oversized line: chunkEnd = %d, want 51 (whole line, not split)", got)
	}
}

// Guards the relationship the default is derived from: base64 inflates the
// JSONL field by 4/3, and the relay rejects bodies over 10 MiB.
func TestMaxIngestChunkFitsRelayBodyLimit(t *testing.T) {
	const relayBodyLimit = 10 << 20
	if encoded := maxIngestChunk * 4 / 3; encoded >= relayBodyLimit {
		t.Fatalf("maxIngestChunk=%d base64-encodes to ~%d bytes, at or over the relay's %d limit",
			maxIngestChunk, encoded, relayBodyLimit)
	}
}
