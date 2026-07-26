// Package sync implements the local-side of arc-sync.
//
// MemoryWatcher tails Claude Code transcript files (~/.claude/projects/**/*.jsonl)
// and POSTs deltas to the relay's /api/memory/ingest endpoint. Runs as a launchd
// (macOS) or systemd (Linux) user service via `arc-sync memory install-service`.
package sync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// maxIngestChunk caps the raw JSONL bytes carried by a single ingest POST.
//
// The relay rejects request bodies over 10 MiB, and ingestRequest.JSONL is a
// []byte that encoding/json base64-encodes, inflating it by ~33% — so the real
// ceiling is ~7.5 MiB of transcript. 4 MiB keeps us clear of that and of the
// parser's 8 MiB per-line scanner limit.
const maxIngestChunk = 4 << 20

// MemoryWatcher walks RootDir for *.jsonl files, POSTs new bytes to BaseURL,
// and persists per-file watermarks in StatePath. Long-running mode uses
// fsnotify with a 5s poll fallback if fsnotify init fails, and a 30s
// belt-and-braces tick regardless.
type MemoryWatcher struct {
	BaseURL    string
	APIKey     string
	RootDir    string
	StatePath  string
	FlagPath   string // mtime change here triggers an immediate scan (Stop hook signal — Task 10)
	HTTPClient *http.Client

	// QuiescenceWindow is the silent period after a successful ingest that
	// signals "session ended" — at which point we POST /api/memory/extract
	// for that session. Zero (default) disables the extract trigger; the
	// cron backstop on the relay still picks them up eventually.
	QuiescenceWindow time.Duration

	// MaxChunkBytes overrides the per-POST ingest chunk size. Zero (default)
	// uses maxIngestChunk. Exists so tests can drive the chunking path without
	// allocating multi-megabyte fixtures.
	MaxChunkBytes int

	mu sync.Mutex
	// quiescenceTimers maps session_id → pending timer. Reset on every
	// ingest; fires PostExtract when the silence threshold is reached.
	quiescenceTimers map[string]*time.Timer
}

type fileState struct {
	BytesSeen int64   `json:"bytes_seen"`
	Mtime     float64 `json:"mtime"`
}

type stateFile struct {
	Files map[string]*fileState `json:"files"`
}

// ingestRequest mirrors memory.IngestRequest on the relay side. Defined here
// (vs imported from the relay package) so arc-sync stays a pure-Go binary
// with no CGO/sqlite dependencies — duplicated wire shape, intentional.
type ingestRequest struct {
	SessionID  string  `json:"session_id"`
	ProjectDir string  `json:"project_dir"`
	FilePath   string  `json:"file_path"`
	FileMtime  float64 `json:"file_mtime"`
	BytesSeen  int64   `json:"bytes_seen"`
	Platform   string  `json:"platform"`
	JSONL      []byte  `json:"jsonl"` // base64-encoded by Go's encoding/json automatically
}

func (w *MemoryWatcher) loadState() *stateFile {
	st := &stateFile{Files: map[string]*fileState{}}
	b, err := os.ReadFile(w.StatePath)
	if err != nil {
		// File missing on first run is expected; only log other errors.
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "memory watch: cannot read state file %s: %v\n", w.StatePath, err)
		}
		return st
	}
	if err := json.Unmarshal(b, st); err != nil {
		fmt.Fprintf(os.Stderr, "memory watch: state file corrupted, starting fresh (%s): %v\n", w.StatePath, err)
		st = &stateFile{Files: map[string]*fileState{}}
	}
	if st.Files == nil {
		st.Files = map[string]*fileState{}
	}
	return st
}

func (w *MemoryWatcher) saveState(st *stateFile) error {
	if err := os.MkdirAll(filepath.Dir(w.StatePath), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(w.StatePath, b, 0o600)
}

// RunOnce performs a single full scan and returns. Used by `memory watch --once`
// and by Run() at startup for catch-up.
func (w *MemoryWatcher) RunOnce() error {
	st := w.loadState()
	return w.scan(st)
}

// Run is the long-running watch loop. fsnotify-driven with a 5s poll fallback
// if fsnotify is unavailable, and a 30s belt-and-braces tick.
func (w *MemoryWatcher) Run() error {
	st := w.loadState()
	if err := w.scan(st); err != nil {
		fmt.Fprintln(os.Stderr, "memory watch initial scan:", err)
	}

	notify, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory watch: fsnotify unavailable, falling back to 5s poll:", err)
		return w.pollLoop(st)
	}
	defer notify.Close()

	if err := w.addRecursive(notify, w.RootDir); err != nil {
		return fmt.Errorf("watch root: %w", err)
	}
	// Also watch the directory containing the wakeup flag — Task 10's Stop hook
	// touches that file to signal an immediate scan. Create the dir first since
	// it's our config dir; if creation fails, log + continue (the 30s tick will
	// still catch up, just not instantly).
	if w.FlagPath != "" {
		flagDir := filepath.Dir(w.FlagPath)
		if err := os.MkdirAll(flagDir, 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "memory watch: cannot create wakeup-flag dir %s: %v\n", flagDir, err)
		} else if err := notify.Add(flagDir); err != nil {
			fmt.Fprintf(os.Stderr, "memory watch: cannot watch wakeup-flag dir %s: %v\n", flagDir, err)
		}
	}

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case ev, ok := <-notify.Events:
			if !ok {
				return nil
			}
			// Trigger scan on any .jsonl change OR a touch of the flag file.
			if !strings.HasSuffix(ev.Name, ".jsonl") && ev.Name != w.FlagPath {
				continue
			}
			if err := w.scan(st); err != nil {
				fmt.Fprintln(os.Stderr, "memory watch scan:", err)
			}
		case err, ok := <-notify.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintln(os.Stderr, "memory watch fsnotify error:", err)
		case <-tick.C:
			if err := w.scan(st); err != nil {
				fmt.Fprintln(os.Stderr, "memory watch tick:", err)
			}
		}
	}
}

func (w *MemoryWatcher) addRecursive(watcher *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // best-effort; transient errors shouldn't stop the watcher
		}
		if info.IsDir() {
			_ = watcher.Add(p)
		}
		return nil
	})
}

func (w *MemoryWatcher) pollLoop(st *stateFile) error {
	for {
		if err := w.scan(st); err != nil {
			fmt.Fprintln(os.Stderr, "memory watch poll:", err)
		}
		time.Sleep(5 * time.Second)
	}
}

func (w *MemoryWatcher) scan(st *stateFile) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	return filepath.Walk(w.RootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		fs := st.Files[path]
		if fs == nil {
			fs = &fileState{}
			st.Files[path] = fs
		}
		size := info.Size()
		mtime := float64(info.ModTime().Unix())
		if size <= fs.BytesSeen && mtime <= fs.Mtime {
			return nil
		}
		delta, err := readTail(path, fs.BytesSeen)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory watch read:", err)
			return nil
		}
		// Ingest only through the last complete line. A transcript that is
		// mid-append ends partway through a record; sending that fragment would
		// hand the parser two malformed halves — it skips both — and the record
		// would be lost for good once the watermark moved past it.
		if cut := bytes.LastIndexByte(delta, '\n'); cut >= 0 {
			delta = delta[:cut+1]
		} else {
			delta = nil
		}
		if len(delta) == 0 {
			fs.Mtime = mtime
			_ = w.saveState(st)
			return nil
		}
		return w.ingestDelta(st, fs, path, delta, mtime)
	})
}

// ingestDelta POSTs delta to the relay in newline-aligned chunks of at most
// maxIngestChunk bytes, advancing the watermark after each one.
//
// Chunking is what stops a large backlog from wedging the watcher permanently.
// A rejected POST must not advance the watermark, so before chunking, a delta
// that exceeded the relay's body limit was re-sent in full on every scan and
// that file never made progress again. Bounded chunks let a backlog of any size
// drain a piece at a time.
func (w *MemoryWatcher) ingestDelta(st *stateFile, fs *fileState, path string, delta []byte, mtime float64) error {
	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	projectDir := decodeProjectDir(filepath.Base(filepath.Dir(path)))
	base := fs.BytesSeen
	messagesAdded := 0
	limit := w.MaxChunkBytes
	if limit <= 0 {
		limit = maxIngestChunk
	}

	for off := 0; off < len(delta); {
		end := chunkEnd(delta, off, limit)
		watermark := base + int64(end)
		body, _ := json.Marshal(&ingestRequest{
			SessionID:  sessionID,
			ProjectDir: projectDir,
			FilePath:   path,
			FileMtime:  mtime,
			BytesSeen:  watermark,
			Platform:   "claude-code",
			JSONL:      delta[off:end],
		})
		resp, err := w.postIngest(body)
		if err != nil {
			// 413 is permanent for these bytes — retrying cannot shrink them.
			// Skip past them so one oversized record can never stall every
			// later record in the file. Anything else (network, auth, 5xx) may
			// succeed later, so leave the watermark for the next scan.
			var httpErr *ingestHTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusRequestEntityTooLarge {
				fmt.Fprintln(os.Stderr, "memory watch ingest:", err)
				return nil
			}
			fmt.Fprintf(os.Stderr, "memory watch: skipping %d bytes at offset %d of %s, relay rejected as too large: %v\n",
				end-off, base+int64(off), path, err)
		} else if resp != nil {
			messagesAdded += resp.MessagesAdded
		}

		off = end
		fs.BytesSeen = watermark
		fs.Mtime = mtime
		if err := w.saveState(st); err != nil {
			return err
		}
	}

	// Phase B: schedule extract POST after a quiescence window. Reset the timer
	// on every ingest for the same session; if no further bytes arrive within
	// the window, fire the extract call. Done once per delta rather than per
	// chunk — each call resets the same timer, so the end state is identical.
	if messagesAdded > 0 {
		w.scheduleQuiescenceExtract(sessionID)
	}
	return nil
}

// chunkEnd returns the exclusive end of the chunk starting at off, always
// landing just past a '\n' so a JSONL record is never split across two POSTs.
// The relay's parser is line-oriented and discards the fragments on both sides
// of a mid-record split, so an unaligned chunk would silently lose data.
//
// A line longer than max is emitted whole rather than cut; if the relay then
// rejects it as too large, the caller skips it.
func chunkEnd(b []byte, off, max int) int {
	if len(b)-off <= max {
		return len(b) // callers pass a delta that already ends on a newline
	}
	if i := bytes.LastIndexByte(b[off:off+max], '\n'); i >= 0 {
		return off + i + 1
	}
	if i := bytes.IndexByte(b[off:], '\n'); i >= 0 {
		return off + i + 1
	}
	return len(b)
}

// scheduleQuiescenceExtract starts (or resets) a per-session timer. When
// the timer fires, we POST /api/memory/extract for that session. If
// QuiescenceWindow is 0, this is a no-op — the relay's cron loop is the
// only extraction trigger.
func (w *MemoryWatcher) scheduleQuiescenceExtract(sessionID string) {
	if w.QuiescenceWindow <= 0 {
		return
	}
	if w.quiescenceTimers == nil {
		w.quiescenceTimers = map[string]*time.Timer{}
	}
	if t, ok := w.quiescenceTimers[sessionID]; ok {
		t.Stop()
	}
	w.quiescenceTimers[sessionID] = time.AfterFunc(w.QuiescenceWindow, func() {
		// Acquire the watcher lock so we don't race with concurrent scans
		// modifying quiescenceTimers.
		w.mu.Lock()
		delete(w.quiescenceTimers, sessionID)
		w.mu.Unlock()

		if err := w.PostExtract(sessionID); err != nil {
			fmt.Fprintln(os.Stderr, "memory watch extract:", err)
			// Cron backstop on the relay will catch this on its next 30 min cycle.
		}
	})
}

// PostExtract POSTs /api/memory/extract for one session. Used by the
// quiescence trigger and by `arc-sync memory extract <session-id>`.
// Returns the relay's response decoded into ExtractResponse, or an error
// if the call failed (network, auth, 4xx/5xx).
func (w *MemoryWatcher) PostExtract(sessionID string) error {
	body, _ := json.Marshal(map[string]string{"session_id": sessionID})
	req, err := http.NewRequest("POST", w.BaseURL+"/api/memory/extract", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.APIKey)
	resp, err := w.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("extract %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// ingestResponse mirrors memory.IngestResponse on the relay side.
type ingestResponse struct {
	MessagesAdded int   `json:"messages_added"`
	EventsAdded   int   `json:"events_added"`
	BytesSeen     int64 `json:"bytes_seen"`
}

// ingestHTTPError is returned when the relay answers an ingest with a 4xx/5xx.
// It carries the status code so callers can tell a permanent rejection (413,
// the body can never get smaller) from one worth retrying. The Error() text
// matches the plain fmt.Errorf string this replaced.
type ingestHTTPError struct {
	StatusCode int
	Body       string
}

func (e *ingestHTTPError) Error() string {
	return fmt.Sprintf("ingest %d: %s", e.StatusCode, e.Body)
}

// postIngest is the renamed-and-extended replacement for the previous
// `post`. Returns the parsed ingest response so callers can decide whether
// to schedule a follow-up extraction.
func (w *MemoryWatcher) postIngest(body []byte) (*ingestResponse, error) {
	req, err := http.NewRequest("POST", w.BaseURL+"/api/memory/ingest", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.APIKey)
	resp, err := w.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		return nil, &ingestHTTPError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(buf))}
	}
	var out ingestResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		// Don't fail the ingest on parse error — the bytes were accepted; we
		// just can't trigger quiescence-based extraction for this delta.
		return &ingestResponse{}, nil
	}
	return &out, nil
}

func readTail(path string, offset int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(f)
}

// decodeProjectDir reverses Claude Code's `/` → `-` escaping in the project
// directory name. The transcript at `~/.claude/projects/-Users-ian-code/abc.jsonl`
// belongs to `/Users/ian/code`. Claude Code encodes `/` as `-` and the resulting
// directory name starts with a leading `-` (from the initial `/`). We strip that
// leading `-` before replacing remaining `-` characters with `/`, then prepend `/`.
//
// Caveat: project directories that legitimately contain `-` (e.g.
// `/Users/ian/my-app`) lose the original hyphens. This is a known Claude Code
// limitation — there's no round-trip-safe encoding in their format. We accept
// the lossy reverse since the value is informational (search filter), not a
// path used for filesystem access.
func decodeProjectDir(escaped string) string {
	// The leading `-` in e.g. `-Users-ian` encodes the root `/`; strip it first.
	stripped := strings.TrimPrefix(escaped, "-")
	return "/" + strings.ReplaceAll(stripped, "-", "/")
}
