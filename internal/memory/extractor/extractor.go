package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/comma-compliance/arc-relay/internal/mcp"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// Backend is the slim interface the extractor needs from the mem0 backend.
// Matches proxy.Backend exactly; abstracted here so tests can fake it.
type Backend interface {
	Send(ctx context.Context, req *mcp.Request) (*mcp.Response, error)
}

// BackendResolver lazily resolves the mem0 (code-memory) MCP backend at call
// time so the extractor survives backend reconnects without restart.
type BackendResolver func() (Backend, bool)

// UsernameResolver maps a relay user_id (UUID) to the user's username — the
// human-readable identifier mem0 uses for its `user_id` field. We resolve at
// extract time (not at session-ingest time) so renames flow through cleanly.
//
// Returns ("", false) if the user can't be resolved; callers fall back to
// sending the UUID, which still works but namespaces the memory differently
// than Claude Code's interactive `mcp__code-memory__*` calls.
type UsernameResolver func(userID string) (string, bool)

// Service orchestrates the extraction pipeline for a single transcript memory
// store. One instance per arc-relay process.
type Service struct {
	sessions       *store.SessionMemoryStore
	messages       *store.MessageStore
	extractions    *store.ExtractionStore
	backend        BackendResolver
	resolveUser    UsernameResolver
	classifier     Classifier // optional; nil disables category metadata
	chunkTarget    int
	requestTimeout time.Duration
	locks          sync.Map // session_id -> *sync.Mutex
}

// SetClassifier wires an optional classifier into the service. When set, every
// chunk gets a `category` field added to its mem0 metadata before sending.
// Pass nil (or never call this) to disable categorization — that keeps
// behavior identical to the pre-classifier version.
func (s *Service) SetClassifier(c Classifier) {
	s.classifier = c
}

// NewService builds an extractor wired to the given stores, backend
// resolver, and username resolver. The backend resolver is called once
// per Extract — return (nil, false) when mem0 isn't registered yet, and
// the call fails fast.
//
// resolveUser may be nil — in which case the extractor sends the relay's
// internal UUID to mem0. Strongly recommended to wire it: otherwise
// extracted memories live under a different mem0 user_id than the user's
// interactive Claude Code memories.
func NewService(sessions *store.SessionMemoryStore, messages *store.MessageStore,
	extractions *store.ExtractionStore, backend BackendResolver, resolveUser UsernameResolver) *Service {
	if resolveUser == nil {
		resolveUser = func(id string) (string, bool) { return "", false }
	}
	return &Service{
		sessions:    sessions,
		messages:    messages,
		extractions: extractions,
		backend:     backend,
		resolveUser: resolveUser,
		// 3000 chars (≈750-1000 tokens) keeps OpenAI's text-embedding-3-*
		// model below its 8192-token input cap with comfortable headroom for
		// mem0's internal concatenation of dedup-candidate metadata.
		chunkTarget: 3000,
		// 180s per add_memory call: production observation showed ~30% of
		// chunks miss the previous 60s budget under load, while mem0's
		// container is far from saturated — the wait is OpenAI latency.
		requestTimeout: 180 * time.Second,
	}
}

// ExtractResult is what Extract returns on success. Counts let the caller
// log/render summary lines without re-querying.
type ExtractResult struct {
	SessionID       string
	MessagesTotal   int
	MessagesKept    int
	MessagesNew     int // kept AND not previously extracted
	ChunksProcessed int
	MemoriesCreated int
	Errors          []string
}

// ErrBackendUnavailable is returned when the mem0 (code-memory) backend
// isn't registered yet. Callers should treat this as transient — the cron
// loop logs and waits for the next cycle; the HTTP handler returns 503.
var ErrBackendUnavailable = errors.New("mem0 backend not registered")

// Extract runs the full pipeline for one session: load → filter → idempotency
// guard → chunk → for each chunk call mem0.add_memory and log provenance.
//
// Concurrency: per-session sync.Mutex serializes overlapping cron + on-demand
// invocations. Cross-session calls run in parallel.
func (s *Service) Extract(ctx context.Context, sessionID string) (*ExtractResult, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id required")
	}

	// Per-session mutex
	lockI, _ := s.locks.LoadOrStore(sessionID, &sync.Mutex{})
	lock := lockI.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	// 1. Load session
	sess, err := s.sessions.Get(sessionID)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}

	// 2. Load messages (full session — idempotency check below skips the
	// already-extracted ones)
	msgs, err := s.messages.GetSession(sessionID, 0)
	if err != nil {
		return nil, fmt.Errorf("get messages: %w", err)
	}

	// 3. Filter (tiers 1-3)
	kept, fstats := Filter(msgs)

	// 4. Idempotency: skip messages already covered by prior extraction rows
	covered, err := s.coveredUUIDs(sessionID)
	if err != nil {
		return nil, fmt.Errorf("idempotency check: %w", err)
	}
	fresh := make([]*store.Message, 0, len(kept))
	for _, m := range kept {
		if m.UUID == "" || !covered[m.UUID] {
			fresh = append(fresh, m)
		}
	}

	result := &ExtractResult{
		SessionID:     sessionID,
		MessagesTotal: fstats.Total,
		MessagesKept:  fstats.KeptCount,
		MessagesNew:   len(fresh),
	}

	// Early return if no new content
	if len(fresh) == 0 {
		now := float64(time.Now().Unix())
		_ = s.sessions.MarkExtracted(sessionID, now)
		slog.Info("extract: nothing new",
			"session", sessionID, "total", fstats.Total, "kept", fstats.KeptCount)
		return result, nil
	}

	// 5. Chunk
	chunks := ChunkMessages(fresh, s.chunkTarget)
	result.ChunksProcessed = len(chunks)

	// 6. Resolve backend lazily — fail before any LLM call if mem0 is down
	backend, ok := s.backend()
	if !ok {
		return nil, ErrBackendUnavailable
	}

	// 7. For each chunk: call mem0.add_memory, record provenance row
	agentID := Derive(sess.ProjectDir)
	now := float64(time.Now().Unix())
	for i, c := range chunks {
		memIDs, callErr := s.callAddMemoryWithRetry(ctx, backend, c, agentID, sess)

		uuidsJSON, _ := json.Marshal(c.UUIDs)
		idsJSON, _ := json.Marshal(memIDs)
		row := &store.MemoryExtraction{
			SessionID:     sessionID,
			ExtractedAt:   now,
			ChunkIndex:    i,
			ChunkMsgUUIDs: string(uuidsJSON),
			ChunkChars:    c.Chars,
			Mem0MemoryIDs: string(idsJSON),
			Mem0Count:     len(memIDs),
		}
		if callErr != nil {
			row.Error.String = callErr.Error()
			row.Error.Valid = true
			result.Errors = append(result.Errors, callErr.Error())
			slog.Warn("extract: chunk failed",
				"session", sessionID,
				"chunk", i,
				"chunk_chars", c.Chars,
				"err", callErr.Error())
		} else {
			result.MemoriesCreated += len(memIDs)
		}
		if insErr := s.extractions.Insert(row); insErr != nil {
			slog.Error("extract: insert provenance row failed",
				"session", sessionID, "chunk", i, "err", insErr)
			// Don't return — continue with remaining chunks; we still want partial progress.
		}
	}

	// 8. Stamp last_extracted_at — even on partial failure cron will only
	// re-pick if last_seen_at advances, and the idempotency guard above
	// won't re-call mem0 for chunks we already inserted rows for.
	_ = s.sessions.MarkExtracted(sessionID, now)

	slog.Info("extract: complete",
		"session", sessionID,
		"total", fstats.Total,
		"kept", fstats.KeptCount,
		"new", len(fresh),
		"chunks", len(chunks),
		"mems", result.MemoriesCreated,
		"errors", len(result.Errors))

	return result, nil
}

// coveredUUIDs returns the set of message UUIDs that have already been
// included in a prior memory_extractions row for this session.
func (s *Service) coveredUUIDs(sessionID string) (map[string]bool, error) {
	rows, err := s.extractions.ListBySession(sessionID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		if r.Error.Valid {
			// Don't treat failed chunks as "covered" — let the next pass
			// retry them.
			continue
		}
		var uuids []string
		if err := json.Unmarshal([]byte(r.ChunkMsgUUIDs), &uuids); err != nil {
			continue
		}
		for _, u := range uuids {
			out[u] = true
		}
	}
	return out, nil
}

// addMemoryArgs is what we send to the code-memory MCP server's add_memory
// tool. The wrapper exposes content/user_id/agent_id/metadata only — it does
// NOT pass through run_id or custom_instructions to mem0. Session ID lives
// in metadata.session_id; mem0 uses its default extractor (no steering).
//
// If extraction quality turns out poor, the lever is to either upgrade the
// code-memory MCP server to expose custom_instructions, or pre-classify on
// our side before sending — both deferred until we see real numbers.
type addMemoryArgs struct {
	Content  string         `json:"content"`
	UserID   string         `json:"user_id"`
	AgentID  string         `json:"agent_id,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// callAddMemoryWithRetry calls mem0's add_memory tool once, retries on
// transient errors (context-deadline-exceeded, context-canceled, SSE
// stream errors). Each attempt gets a fresh requestTimeout context so the
// retry has a full deadline budget.
//
// Two attempts max — if both fail, the chunk's row is written with the
// final error and the cron loop will retry next time `last_seen_at`
// advances.
func (s *Service) callAddMemoryWithRetry(ctx context.Context, backend Backend, c Chunk,
	agentID string, sess *store.MemorySession) ([]string, error) {

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		// Bail out if the parent context is already cancelled — no point
		// burning a fresh per-call deadline on a doomed call.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		callCtx, cancel := context.WithTimeout(ctx, s.requestTimeout)
		memIDs, err := s.callAddMemory(callCtx, backend, c, agentID, sess)
		cancel()
		if err == nil {
			return memIDs, nil
		}
		lastErr = err
		if !isRetryable(err) {
			return nil, err
		}
		// Small jittered backoff so a thundering retry doesn't compound the
		// load that caused the timeout in the first place.
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return nil, lastErr
}

// isRetryable returns true for errors that are likely to succeed on a
// retry — transient timeouts, context cancellations, and partial-stream
// errors. Real protocol errors (mem0 4xx, malformed responses) are not
// retried.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "context deadline exceeded"):
		return true
	case strings.Contains(msg, "context canceled"):
		return true
	case strings.Contains(msg, "reading SSE stream"):
		return true
	case strings.Contains(msg, "EOF"):
		return true
	}
	return false
}

// callAddMemory issues one tools/call against the code-memory MCP backend
// and parses out the returned mem0 memory IDs.
func (s *Service) callAddMemory(ctx context.Context, backend Backend, c Chunk,
	agentID string, sess *store.MemorySession) ([]string, error) {

	// Resolve username so mem0 stores under the human-readable user_id ("ian")
	// and shares a namespace with the user's interactive code-memory calls.
	// Falls back to the relay UUID if the resolver can't find the user.
	userID := sess.UserID
	if name, ok := s.resolveUser(sess.UserID); ok && name != "" {
		userID = name
	}

	metadata := map[string]any{
		"project_dir":      sess.ProjectDir,
		"session_id":       sess.SessionID,
		"platform":         sess.Platform,
		"last_seen_at":     sess.LastSeenAt,
		"source_msg_uuids": c.UUIDs,
	}

	// Optional category from the LLM classifier. We deliberately don't fail
	// the extraction on classifier errors — categorization is a nice-to-have,
	// the underlying extraction is what matters. A classifier outage just
	// produces uncategorized memories until the classifier comes back.
	if s.classifier != nil {
		clsCtx, clsCancel := context.WithTimeout(ctx, 30*time.Second)
		category, err := s.classifier.Classify(clsCtx, c.Text)
		clsCancel()
		if err != nil {
			slog.Warn("classifier failed",
				"session", sess.SessionID, "err", err)
		} else if category != "" {
			metadata["category"] = category
		}
	}

	args := addMemoryArgs{
		Content:  c.Text,
		UserID:   userID,
		AgentID:  agentID,
		Metadata: metadata,
	}

	params := map[string]any{
		"name":      "add_memory",
		"arguments": args,
	}
	idRaw, _ := json.Marshal("extract-" + sess.SessionID)
	req, err := mcp.NewRequest(idRaw, "tools/call", params)
	if err != nil {
		return nil, fmt.Errorf("build mcp request: %w", err)
	}

	resp, err := backend.Send(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mem0 send: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mem0 error %d: %s", resp.Error.Code, resp.Error.Message)
	}

	return parseMemoryIDs(resp.Result), nil
}

// MemoryHit is one mem0 search result, normalized for /recall blending.
// Mirrors the most useful fields from mem0's response — full payload is
// available via the existing /mcp/code-memory MCP route.
type MemoryHit struct {
	ID         string  `json:"id"`
	AgentID    string  `json:"agent_id"`
	Memory     string  `json:"memory"`
	Score      float64 `json:"score"`
	SessionID  string  `json:"session_id,omitempty"`
	ProjectDir string  `json:"project_dir,omitempty"`
	LastSeenAt float64 `json:"last_seen_at,omitempty"`
}

// SearchTranscriptMemories runs `mem0.search_memories` for one user and
// returns only the memories whose agent_id starts with "transcripts-" —
// i.e., the ones produced by this pipeline. Used by the blended /recall
// path to mix distilled memories alongside FTS5 transcript hits.
//
// Returns an empty slice if the backend is unavailable. Network errors
// bubble up so /recall can show "memory backend unreachable" without
// failing the whole search.
func (s *Service) SearchTranscriptMemories(ctx context.Context, userID, query string, limit int) ([]MemoryHit, error) {
	if userID == "" || query == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	backend, ok := s.backend()
	if !ok {
		return nil, nil // mem0 not wired; just no memory hits
	}

	// Map relay UUID → username so we hit the same mem0 namespace the
	// extractor wrote into.
	mem0UserID := userID
	if name, ok := s.resolveUser(userID); ok && name != "" {
		mem0UserID = name
	}

	args := map[string]any{
		"query":   query,
		"user_id": mem0UserID,
		"limit":   limit,
	}
	params := map[string]any{
		"name":      "search_memories",
		"arguments": args,
	}
	idRaw, _ := json.Marshal("recall-search")
	req, err := mcp.NewRequest(idRaw, "tools/call", params)
	if err != nil {
		return nil, fmt.Errorf("build mcp request: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := backend.Send(callCtx, req)
	if err != nil {
		return nil, fmt.Errorf("mem0 search: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mem0 error %d: %s", resp.Error.Code, resp.Error.Message)
	}

	return parseMemoryHits(resp.Result), nil
}

// parseMemoryHits decodes mem0 search_memories tool output into MemoryHit
// records, dropping anything whose agent_id doesn't look like ours.
func parseMemoryHits(result json.RawMessage) []MemoryHit {
	if len(result) == 0 {
		return nil
	}
	var wrapper struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &wrapper); err != nil || len(wrapper.Content) == 0 {
		return nil
	}
	text := wrapper.Content[0].Text
	if text == "" {
		return nil
	}

	type rawHit struct {
		ID       string         `json:"id"`
		AgentID  string         `json:"agent_id"`
		Memory   string         `json:"memory"`
		Score    float64        `json:"score"`
		Metadata map[string]any `json:"metadata"`
	}

	// mem0 wraps results either as {"results":[...]} or [...].
	var arr []rawHit
	var withResults struct {
		Results []rawHit `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &withResults); err == nil && len(withResults.Results) > 0 {
		arr = withResults.Results
	} else if err := json.Unmarshal([]byte(text), &arr); err != nil {
		return nil
	}

	out := make([]MemoryHit, 0, len(arr))
	for _, h := range arr {
		if !IsTranscriptAgentID(h.AgentID) {
			continue
		}
		hit := MemoryHit{
			ID:      h.ID,
			AgentID: h.AgentID,
			Memory:  h.Memory,
			Score:   h.Score,
		}
		if h.Metadata != nil {
			if v, ok := h.Metadata["session_id"].(string); ok {
				hit.SessionID = v
			}
			if v, ok := h.Metadata["project_dir"].(string); ok {
				hit.ProjectDir = v
			}
			if v, ok := h.Metadata["last_seen_at"].(float64); ok {
				hit.LastSeenAt = v
			}
		}
		out = append(out, hit)
	}
	return out
}

// parseMemoryIDs extracts memory IDs from a tools/call result. The MCP
// content shape is `{"content":[{"type":"text","text":"..."}]}`; the inner
// text is whatever the server wraps in. mem0's code-memory wrapper returns
// JSON that may be one of:
//
//	{"results":[{"id":"...","memory":"..."},...]}
//	[{"id":"...","memory":"..."},...]
//	{"id":"...","memory":"..."}            // single result
//
// Anything else returns an empty slice — we log the count, not the error,
// because mem0 sometimes legitimately decides "no memories" for a chunk.
func parseMemoryIDs(result json.RawMessage) []string {
	if len(result) == 0 {
		return nil
	}
	var wrapper struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &wrapper); err != nil || len(wrapper.Content) == 0 {
		return nil
	}
	text := wrapper.Content[0].Text
	if text == "" {
		return nil
	}

	// Try {"results":[...]}
	var withResults struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &withResults); err == nil && len(withResults.Results) > 0 {
		out := make([]string, 0, len(withResults.Results))
		for _, r := range withResults.Results {
			if r.ID != "" {
				out = append(out, r.ID)
			}
		}
		if len(out) > 0 {
			return out
		}
	}

	// Try [...]
	var arr []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &arr); err == nil && len(arr) > 0 {
		out := make([]string, 0, len(arr))
		for _, r := range arr {
			if r.ID != "" {
				out = append(out, r.ID)
			}
		}
		if len(out) > 0 {
			return out
		}
	}

	// Try single object {"id":"..."}
	var single struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &single); err == nil && single.ID != "" {
		return []string{single.ID}
	}

	return nil
}
