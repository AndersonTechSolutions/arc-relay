// Package sync implements the local-side of arc-sync.
//
// MemoryWatcher tails Claude Code transcript files (~/.claude/projects/**/*.jsonl)
// and POSTs deltas to the relay's /api/memory/ingest endpoint. Runs as a launchd
// (macOS) or systemd (Linux) user service via `arc-sync memory install-service`.
//
// Phase 0 hardening (spec 2026-09-17 §4.2): versioned state with atomic
// writes and a single-writer lock, directory discovery after startup,
// bounded reads with a hard record ceiling, 413 halving, 409 quarantine, and
// per-file counters that ride every ingest.
package sync

import (
	"bytes"
	"context"
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

// maxRecordBytes is the hard ceiling on one JSONL line. A longer line is
// skipped by scanning to the next newline without ever buffering it, and
// counted as skipped_records so the loss is visible in relay stats.
const maxRecordBytes = 64 << 20

// rawCwdHeadBytes bounds the head-read that discovers a transcript's own cwd.
const rawCwdHeadBytes = 64 << 10

const stateVersion = 2

// MemoryWatcher walks RootDir for *.jsonl files, POSTs new bytes to BaseURL,
// and persists per-file watermarks in the v2 state file next to StatePath.
// Long-running mode uses fsnotify with a 5s poll fallback if fsnotify init
// fails, and a 30s belt-and-braces tick that also reconciles directory watches.
type MemoryWatcher struct {
	BaseURL string
	APIKey  string
	RootDir string
	// StatePath is the legacy (v1) state path. The watcher reads and writes
	// the sibling "<name>.v2.json"; v1 is imported once and never written
	// again, so a downgraded watcher resumes from its own last watermarks.
	StatePath  string
	FlagPath   string // mtime change here triggers an immediate scan (Stop hook signal)
	HTTPClient *http.Client

	// QuiescenceWindow is the silent period after a successful ingest that
	// signals "session ended" — at which point we POST /api/memory/extract
	// for that session. Zero (default) disables the extract trigger; the
	// cron backstop on the relay still picks them up eventually.
	QuiescenceWindow time.Duration

	// MaxChunkBytes overrides the per-POST ingest chunk size (and the read
	// window). Zero (default) uses maxIngestChunk. Exists so tests can drive
	// the chunking path without allocating multi-megabyte fixtures.
	MaxChunkBytes int

	// RecordCeiling overrides maxRecordBytes. Zero (default) uses it.
	RecordCeiling int64

	// ResetState discards an unreadable or unknown-version state file and
	// starts from empty. Without it the watcher refuses to start, because a
	// silent reset replays every transcript from byte 0.
	ResetState bool

	// Platform is the source key in state and on the wire ("claude-code").
	Platform string

	mu sync.Mutex
	// quiescenceTimers maps session_id → pending timer. Reset on every
	// ingest; fires PostExtract when the silence threshold is reached.
	quiescenceTimers map[string]*time.Timer
}

// fileState is the per-transcript watermark plus Phase 0 bookkeeping.
type fileState struct {
	BytesSeen int64   `json:"bytes_seen"`
	Mtime     float64 `json:"mtime"`
	// RawCwd is the transcript's own cwd (head-read once), sent as raw_cwd.
	RawCwd string `json:"raw_cwd,omitempty"`
	// Quarantined is the reason this file is no longer read (e.g. the relay
	// answered 409: the session belongs to another user).
	Quarantined string `json:"quarantined,omitempty"`
	// Counters are diagnostics that ride every ingest as client_counters.
	Counters map[string]int64 `json:"counters,omitempty"`
}

func (fs *fileState) count(name string) {
	if fs.Counters == nil {
		fs.Counters = map[string]int64{}
	}
	fs.Counters[name]++
}

type sourceState struct {
	Files map[string]*fileState `json:"files"`
}

// stateFile is the v2 on-disk shape: watermarks are grouped per platform so
// later sources (codex, …) get their own namespace.
type stateFile struct {
	Version int                     `json:"version"`
	Sources map[string]*sourceState `json:"sources"`
}

// stateFileV1 is the pre-Phase-0 shape, read once for import.
type stateFileV1 struct {
	Files map[string]*fileState `json:"files"`
}

func newStateFile() *stateFile {
	return &stateFile{Version: stateVersion, Sources: map[string]*sourceState{}}
}

// files returns the per-file map for platform, creating it if needed.
func (st *stateFile) files(platform string) map[string]*fileState {
	if st.Sources == nil {
		st.Sources = map[string]*sourceState{}
	}
	src := st.Sources[platform]
	if src == nil {
		src = &sourceState{}
		st.Sources[platform] = src
	}
	if src.Files == nil {
		src.Files = map[string]*fileState{}
	}
	return src.Files
}

// ingestRequest mirrors memory.IngestRequest on the relay side. Defined here
// (vs imported from the relay package) so arc-sync stays a pure-Go binary
// with no CGO/sqlite dependencies — duplicated wire shape, intentional.
type ingestRequest struct {
	SessionID      string           `json:"session_id"`
	ProjectDir     string           `json:"project_dir"`
	FilePath       string           `json:"file_path"`
	FileMtime      float64          `json:"file_mtime"`
	BytesSeen      int64            `json:"bytes_seen"`
	Platform       string           `json:"platform"`
	JSONL          []byte           `json:"jsonl"` // base64-encoded by Go's encoding/json automatically
	RawCwd         string           `json:"raw_cwd,omitempty"`
	ClientCounters map[string]int64 `json:"client_counters,omitempty"`
}

func (w *MemoryWatcher) platform() string {
	if w.Platform == "" {
		return "claude-code"
	}
	return w.Platform
}

func (w *MemoryWatcher) chunkLimit() int {
	if w.MaxChunkBytes > 0 {
		return w.MaxChunkBytes
	}
	return maxIngestChunk
}

func (w *MemoryWatcher) recordCeiling() int64 {
	if w.RecordCeiling > 0 {
		return w.RecordCeiling
	}
	return maxRecordBytes
}

// statePathV2 is StatePath with ".json" replaced by ".v2.json".
func (w *MemoryWatcher) statePathV2() string {
	if strings.HasSuffix(w.StatePath, ".json") {
		return strings.TrimSuffix(w.StatePath, ".json") + ".v2.json"
	}
	return w.StatePath + ".v2"
}

// lockPath is the single-writer lock next to the state file.
func (w *MemoryWatcher) lockPath() string {
	return strings.TrimSuffix(w.StatePath, ".json") + ".lock"
}

// loadState reads the v2 state, importing v1 on first run. An unreadable or
// unknown-version file is an error unless ResetState is set: silently
// starting empty would replay every transcript from byte 0.
func (w *MemoryWatcher) loadState() (*stateFile, error) {
	v2 := w.statePathV2()
	if b, err := os.ReadFile(v2); err == nil { // #nosec G304 -- own config dir
		st := &stateFile{}
		if jerr := json.Unmarshal(b, st); jerr != nil || st.Version != stateVersion {
			if !w.ResetState {
				return nil, fmt.Errorf("memory watch: state file %s is unreadable or not version %d (%v); rerun with --reset-state to start from empty (every transcript will be re-sent; the relay deduplicates)", v2, stateVersion, jerr)
			}
			fmt.Fprintf(os.Stderr, "memory watch: --reset-state: discarding %s\n", v2)
			return newStateFile(), nil
		}
		if st.Sources == nil {
			st.Sources = map[string]*sourceState{}
		}
		return st, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("memory watch: cannot read state file %s: %w", v2, err)
	}

	// No v2 yet: import v1 once. v1 is never rewritten.
	st := newStateFile()
	b, err := os.ReadFile(w.StatePath) // #nosec G304 -- own config dir
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory watch: cannot read legacy state file %s: %w", w.StatePath, err)
	}
	var v1 stateFileV1
	if jerr := json.Unmarshal(b, &v1); jerr != nil {
		if !w.ResetState {
			return nil, fmt.Errorf("memory watch: legacy state file %s is corrupt (%v); rerun with --reset-state to start from empty", w.StatePath, jerr)
		}
		fmt.Fprintf(os.Stderr, "memory watch: --reset-state: ignoring corrupt legacy state %s\n", w.StatePath)
		return st, nil
	}
	files := st.files(w.platform())
	for p, fs := range v1.Files {
		if fs != nil {
			files[p] = fs
		}
	}
	if err := w.saveState(st); err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "memory watch: imported %d watermarks from %s into %s\n", len(files), w.StatePath, v2)
	return st, nil
}

// saveState writes the v2 state atomically: temp file, fsync, rename. A crash
// mid-write leaves the previous state intact instead of a truncated file.
func (w *MemoryWatcher) saveState(st *stateFile) error {
	path := w.statePathV2()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	st.Version = stateVersion
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- own config dir
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// RunOnce performs a single full scan and returns. Used by `memory watch --once`
// and by Run() at startup for catch-up.
func (w *MemoryWatcher) RunOnce() error {
	lock, err := acquireStateLock(w.lockPath())
	if err != nil {
		return err
	}
	defer lock.release()
	st, err := w.loadState()
	if err != nil {
		return err
	}
	return w.scan(st)
}

// Run is the long-running watch loop with a background context.
func (w *MemoryWatcher) Run() error {
	return w.RunContext(context.Background())
}

// RunContext is the long-running watch loop. fsnotify-driven with a 5s poll
// fallback if fsnotify is unavailable, and a 30s belt-and-braces tick that
// also reconciles directory watches (new project dirs, roots created after
// startup). Returns when ctx is done.
func (w *MemoryWatcher) RunContext(ctx context.Context) error {
	lock, err := acquireStateLock(w.lockPath())
	if err != nil {
		return err
	}
	defer lock.release()

	st, err := w.loadState()
	if err != nil {
		return err
	}
	if err := w.scan(st); err != nil {
		fmt.Fprintln(os.Stderr, "memory watch initial scan:", err)
	}

	notify, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory watch: fsnotify unavailable, falling back to 5s poll:", err)
		return w.pollLoop(ctx, st)
	}
	defer func() { _ = notify.Close() }()

	w.addRecursive(notify, w.RootDir)
	// Also watch the directory containing the wakeup flag — the Stop hook
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
		case <-ctx.Done():
			return nil
		case ev, ok := <-notify.Events:
			if !ok {
				return nil
			}
			// A new directory (Claude creates one per project) must be
			// watched before its files are, or their events never arrive.
			if ev.Has(fsnotify.Create) {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					w.addRecursive(notify, ev.Name)
					if err := w.scan(st); err != nil {
						fmt.Fprintln(os.Stderr, "memory watch scan:", err)
					}
					continue
				}
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
			// Reconcile: directories that appeared without an event we
			// caught (or a root created after startup) get watched now.
			w.addRecursive(notify, w.RootDir)
			if err := w.scan(st); err != nil {
				fmt.Fprintln(os.Stderr, "memory watch tick:", err)
			}
		}
	}
}

// addRecursive watches root and every directory below it. Adding an already
// watched directory is a no-op in fsnotify, so it is safe to call repeatedly.
func (w *MemoryWatcher) addRecursive(watcher *fsnotify.Watcher, root string) {
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // best-effort; transient errors shouldn't stop the watcher
		}
		if info.IsDir() {
			_ = watcher.Add(p)
		}
		return nil
	})
}

func (w *MemoryWatcher) pollLoop(ctx context.Context, st *stateFile) error {
	for {
		if err := w.scan(st); err != nil {
			fmt.Fprintln(os.Stderr, "memory watch poll:", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

// scan walks RootDir and drains every transcript with new bytes, one bounded
// window at a time. Files quarantined by the relay (409) are skipped.
func (w *MemoryWatcher) scan(st *stateFile) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	files := st.files(w.platform())
	return filepath.Walk(w.RootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		fs := files[path]
		if fs == nil {
			fs = &fileState{}
			files[path] = fs
		}
		if fs.Quarantined != "" {
			return nil
		}
		size := info.Size()
		mtime := float64(info.ModTime().Unix())
		if size <= fs.BytesSeen && mtime <= fs.Mtime {
			return nil
		}
		if fs.RawCwd == "" {
			fs.RawCwd = readRawCwd(path)
		}
		if err := w.drainFile(st, fs, path, size, mtime); err != nil {
			return err
		}
		return nil
	})
}

// drainFile reads the unseen part of one transcript in windows of at most the
// chunk limit, so a large backlog never lands in memory at once. A line that
// does not fit a window is measured without buffering: if it is within the
// record ceiling it is sent on its own, otherwise it is skipped and counted.
func (w *MemoryWatcher) drainFile(st *stateFile, fs *fileState, path string, size int64, mtime float64) error {
	limit := int64(w.chunkLimit())
	for fs.BytesSeen < size {
		window, err := readWindow(path, fs.BytesSeen, limit)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory watch read:", err)
			return nil
		}
		if len(window) == 0 {
			break
		}
		cut := bytes.LastIndexByte(window, '\n')
		if cut >= 0 {
			// Ingest only through the last complete line. A transcript that is
			// mid-append ends partway through a record; sending that fragment
			// would hand the parser two malformed halves.
			advanced, err := w.ingestDelta(st, fs, path, window[:cut+1], mtime)
			if err != nil {
				return err
			}
			if !advanced || fs.Quarantined != "" {
				return nil
			}
			continue
		}
		if int64(len(window)) < limit {
			// Reached EOF inside an incomplete trailing line: wait for more.
			fs.Mtime = mtime
			return w.saveState(st)
		}
		// One line is longer than the window. Measure it without buffering.
		lineLen, complete, err := measureLine(path, fs.BytesSeen)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory watch read:", err)
			return nil
		}
		if !complete {
			fs.Mtime = mtime
			return w.saveState(st)
		}
		if lineLen > w.recordCeiling() {
			fmt.Fprintf(os.Stderr, "memory watch: skipping one %d-byte record at offset %d of %s (over the %d-byte ceiling)\n",
				lineLen, fs.BytesSeen, path, w.recordCeiling())
			fs.count("skipped_records")
			fs.BytesSeen += lineLen
			fs.Mtime = mtime
			if err := w.saveState(st); err != nil {
				return err
			}
			continue
		}
		line, err := readWindow(path, fs.BytesSeen, lineLen)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory watch read:", err)
			return nil
		}
		advanced, err := w.ingestDelta(st, fs, path, line, mtime)
		if err != nil {
			return err
		}
		if !advanced || fs.Quarantined != "" {
			return nil
		}
	}
	return nil
}

// ingestDelta POSTs delta to the relay in newline-aligned chunks of at most
// the chunk limit, advancing the watermark after each settled chunk. Returns
// whether the watermark moved; a transient error leaves it for the next scan.
func (w *MemoryWatcher) ingestDelta(st *stateFile, fs *fileState, path string, delta []byte, mtime float64) (bool, error) {
	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	projectDir := decodeProjectDir(filepath.Base(filepath.Dir(path)))
	base := fs.BytesSeen
	messagesAdded := 0
	advanced := false
	limit := w.chunkLimit()

	for off := 0; off < len(delta); {
		end := chunkEnd(delta, off, limit)
		settled, added := w.sendUnit(fs, path, sessionID, projectDir, delta[off:end], base+int64(off), mtime)
		if !settled {
			if fs.Quarantined != "" {
				// The watermark stays, but the quarantine must survive a
				// restart or the next scan retries the same bytes forever.
				if err := w.saveState(st); err != nil {
					return advanced, err
				}
			}
			return advanced, nil
		}
		messagesAdded += added
		off = end
		fs.BytesSeen = base + int64(end)
		fs.Mtime = mtime
		advanced = true
		if err := w.saveState(st); err != nil {
			return advanced, err
		}
		if fs.Quarantined != "" {
			break
		}
	}

	// Phase B: schedule extract POST after a quiescence window. Reset the timer
	// on every ingest for the same session; if no further bytes arrive within
	// the window, fire the extract call. Done once per delta rather than per
	// chunk — each call resets the same timer, so the end state is identical.
	if messagesAdded > 0 {
		w.scheduleQuiescenceExtract(sessionID)
	}
	return advanced, nil
}

// sendUnit POSTs one newline-aligned unit starting at absolute file offset
// off. It reports whether the bytes are settled — accepted, or permanently
// skipped — so the caller can advance past them. A transient error (network,
// auth, 5xx) is not settled. 413 on a multi-record unit is retried as two
// halves; a single record that is still too large is skipped and counted.
// 409 quarantines the file: the session belongs to another relay user.
func (w *MemoryWatcher) sendUnit(fs *fileState, path, sessionID, projectDir string, unit []byte, off int64, mtime float64) (bool, int) {
	body, _ := json.Marshal(&ingestRequest{
		SessionID:      sessionID,
		ProjectDir:     projectDir,
		FilePath:       path,
		FileMtime:      mtime,
		BytesSeen:      off + int64(len(unit)),
		Platform:       w.platform(),
		JSONL:          unit,
		RawCwd:         fs.RawCwd,
		ClientCounters: fs.Counters,
	})
	resp, err := w.postIngest(body)
	if err == nil {
		if resp == nil {
			return true, 0
		}
		if resp.RejectedRows > 0 {
			fs.Counters = addCount(fs.Counters, "rejected_records", int64(resp.RejectedRows))
			fmt.Fprintf(os.Stderr, "memory watch: relay rejected %d normalized rows from %s\n", resp.RejectedRows, path)
		}
		return true, resp.MessagesAdded
	}
	var httpErr *ingestHTTPError
	if !errors.As(err, &httpErr) {
		fmt.Fprintln(os.Stderr, "memory watch ingest:", err)
		return false, 0
	}
	switch httpErr.StatusCode {
	case http.StatusRequestEntityTooLarge:
		if bytes.Count(unit, []byte{'\n'}) > 1 {
			// Halve at the newline nearest the middle and retry each half:
			// the limit may sit below our chunk size (a proxy in front of
			// the relay), and one oversized record must not take its
			// neighbours down with it.
			a, b := halveAtNewline(unit)
			okA, nA := w.sendUnit(fs, path, sessionID, projectDir, a, off, mtime)
			if !okA {
				return false, 0
			}
			okB, nB := w.sendUnit(fs, path, sessionID, projectDir, b, off+int64(len(a)), mtime)
			return okB, nA + nB
		}
		fmt.Fprintf(os.Stderr, "memory watch: skipping %d bytes at offset %d of %s, relay rejected as too large: %v\n",
			len(unit), off, path, err)
		fs.count("skipped_records")
		return true, 0
	case http.StatusConflict:
		if fs.Quarantined == "" {
			fmt.Fprintf(os.Stderr, "memory watch: quarantining %s — relay says session %s belongs to another user (%v)\n",
				path, sessionID, err)
		}
		fs.Quarantined = "ownership_conflict"
		fs.count("ownership_conflicts")
		return false, 0
	default:
		fmt.Fprintln(os.Stderr, "memory watch ingest:", err)
		return false, 0
	}
}

func addCount(m map[string]int64, name string, n int64) map[string]int64 {
	if m == nil {
		m = map[string]int64{}
	}
	m[name] += n
	return m
}

// halveAtNewline splits a multi-line unit at the newline nearest its middle.
func halveAtNewline(unit []byte) ([]byte, []byte) {
	mid := len(unit) / 2
	i := bytes.LastIndexByte(unit[:mid], '\n')
	if i < 0 || i == len(unit)-1 {
		if j := bytes.IndexByte(unit[mid:], '\n'); j >= 0 && mid+j < len(unit)-1 {
			i = mid + j
		} else if i < 0 {
			return unit, nil
		}
	}
	return unit[:i+1], unit[i+1:]
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

		res, err := w.PostExtractMode(sessionID, ExtractModeAuto)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory watch extract:", err)
			// Cron backstop on the relay will catch this on its next 30 min cycle.
			return
		}
		if !res.Queued {
			// Expected for sessions the relay's gate declines (platform, age,
			// still busy, subagent). Not an error; cron re-evaluates later.
			fmt.Fprintf(os.Stderr, "memory watch extract: %s not queued (%s)\n", sessionID, res.Reason)
		}
	})
}

// Extract modes understood by /api/memory/extract.
const (
	ExtractModeAuto   = "auto"
	ExtractModeManual = "manual"
)

// ExtractResponse is the relay's answer to /api/memory/extract.
type ExtractResponse struct {
	SessionID string `json:"session_id"`
	Mode      string `json:"mode"`
	Queued    bool   `json:"queued"`
	Reason    string `json:"reason"`
}

// PostExtract POSTs an automatic extraction request for one session.
func (w *MemoryWatcher) PostExtract(sessionID string) error {
	_, err := w.PostExtractMode(sessionID, ExtractModeAuto)
	return err
}

// PostExtractMode POSTs /api/memory/extract with the given mode. Manual mode
// (used by `arc-sync memory extract <id>`) skips the relay's age/platform/
// quiet gates but not the subagent rule, and is best-effort across relay
// restarts. Returns the relay's decoded response, or an error on 4xx/5xx.
func (w *MemoryWatcher) PostExtractMode(sessionID, mode string) (*ExtractResponse, error) {
	body, _ := json.Marshal(map[string]string{"session_id": sessionID, "mode": mode})
	req, err := http.NewRequest("POST", w.BaseURL+"/api/memory/extract", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.APIKey)
	resp, err := w.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("extract %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	out := &ExtractResponse{SessionID: sessionID, Mode: mode}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil || (!out.Queued && out.Reason == "") {
		// Pre-Phase-0 relays answer {"status":"accepted"} with no queued or
		// reason field; treat that as queued so the caller does not log noise.
		out.Queued = true
		out.Reason = "accepted"
	}
	return out, nil
}

// ingestResponse mirrors memory.IngestResponse on the relay side.
type ingestResponse struct {
	MessagesAdded int   `json:"messages_added"`
	EventsAdded   int   `json:"events_added"`
	BytesSeen     int64 `json:"bytes_seen"`
	RejectedRows  int   `json:"rejected_rows"`
}

// ingestHTTPError is returned when the relay answers an ingest with a 4xx/5xx.
// It carries the status code so callers can tell a permanent rejection (413,
// the body can never get smaller; 409, the session is not ours) from one
// worth retrying. The Error() text matches the plain fmt.Errorf string this
// replaced.
type ingestHTTPError struct {
	StatusCode int
	Body       string
}

func (e *ingestHTTPError) Error() string {
	return fmt.Sprintf("ingest %d: %s", e.StatusCode, e.Body)
}

// postIngest POSTs one ingest body and returns the parsed response so callers
// can decide whether to schedule a follow-up extraction.
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
	defer func() { _ = resp.Body.Close() }()
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

// readWindow reads up to n bytes of path starting at offset. It returns fewer
// bytes only at EOF.
func readWindow(path string, offset, n int64) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- transcript path from our own walk
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, n)
	read, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:read], nil
}

// measureLine returns the length (including the newline) of the line that
// starts at offset, scanning in fixed blocks so an arbitrarily long line is
// never held in memory. complete is false when EOF arrives before a newline.
func measureLine(path string, offset int64) (n int64, complete bool, err error) {
	f, err := os.Open(path) // #nosec G304 -- transcript path from our own walk
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, false, err
	}
	buf := make([]byte, 1<<20)
	for {
		read, rerr := f.Read(buf)
		if read > 0 {
			if i := bytes.IndexByte(buf[:read], '\n'); i >= 0 {
				return n + int64(i) + 1, true, nil
			}
			n += int64(read)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return n, false, nil
			}
			return 0, false, rerr
		}
	}
}

// readRawCwd returns the transcript's own working directory from the first
// record in its head that carries a "cwd" field, or "" if none is found in
// the first rawCwdHeadBytes. Claude Code writes cwd on every message record.
func readRawCwd(path string) string {
	head, err := readWindow(path, 0, rawCwdHeadBytes)
	if err != nil {
		return ""
	}
	for _, line := range bytes.Split(head, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.Cwd != "" {
			return rec.Cwd
		}
	}
	return ""
}

// decodeProjectDir reverses Claude Code's `/` → `-` escaping in the project
// directory name. The transcript at `~/.claude/projects/-Users-ian-code/abc.jsonl`
// belongs to `/Users/ian/code`. Claude Code encodes `/` as `-` and the resulting
// directory name starts with a leading `-` (from the initial `/`). We strip that
// leading `-` before replacing remaining `-` characters with `/`, then prepend `/`.
//
// Caveat: project directories that legitimately contain `-` (e.g.
// `/Users/ian/my-app`) lose the original hyphens. This is a known Claude Code
// limitation — there's no round-trip-safe encoding in their format. The
// transcript's own cwd (raw_cwd) is the authoritative value; this stays as
// the informational project_dir the dashboard groups by.
func decodeProjectDir(escaped string) string {
	// The leading `-` in e.g. `-Users-ian` encodes the root `/`; strip it first.
	stripped := strings.TrimPrefix(escaped, "-")
	return "/" + strings.ReplaceAll(stripped, "-", "/")
}
