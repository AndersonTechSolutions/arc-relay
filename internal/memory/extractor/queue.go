package extractor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/comma-compliance/arc-relay/internal/store"
)

// Mode says who asked for an extraction. Automatic requests (watcher
// quiescence, cron) must pass the eligibility gate; manual requests
// (`arc-sync memory extract <id>`) skip the age/platform/quiet gates but
// never the subagent rule, and are best-effort across restarts.
type Mode string

const (
	ModeAuto   Mode = "auto"
	ModeManual Mode = "manual"
)

// QueueConfig bounds the single extraction admission queue (spec P0-S5).
type QueueConfig struct {
	Workers     int
	Capacity    int
	PassTimeout time.Duration
	Eligibility store.EligibilityConfig
}

// DefaultQueueConfig returns the spec defaults: 2 workers, capacity 1,000,
// 15-minute pass timeout, default eligibility.
func DefaultQueueConfig() QueueConfig {
	return QueueConfig{
		Workers:     2,
		Capacity:    1000,
		PassTimeout: 15 * time.Minute,
		Eligibility: store.DefaultEligibilityConfig(),
	}
}

type queueItem struct {
	sessionID string
	mode      Mode
}

// Queue is the one path every extraction takes. Sessions are deduplicated
// across queued and running, eligibility is re-checked at dequeue, and each
// pass runs under PassTimeout.
type Queue struct {
	svc *Service
	cfg QueueConfig
	ch  chan queueItem

	mu      sync.Mutex
	pending map[string]struct{}
}

// StartQueue creates the queue, registers it on the service and starts the
// workers. They exit when ctx is done.
func (s *Service) StartQueue(ctx context.Context, cfg QueueConfig) *Queue {
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.Capacity <= 0 {
		cfg.Capacity = 1000
	}
	if cfg.PassTimeout <= 0 {
		cfg.PassTimeout = 15 * time.Minute
	}
	q := &Queue{
		svc:     s,
		cfg:     cfg,
		ch:      make(chan queueItem, cfg.Capacity),
		pending: map[string]struct{}{},
	}
	s.queue = q
	for i := 0; i < cfg.Workers; i++ {
		go q.worker(ctx, i)
	}
	slog.Info("extraction queue started",
		"workers", cfg.Workers, "capacity", cfg.Capacity,
		"pass_timeout", cfg.PassTimeout, "platforms", cfg.Eligibility.Platforms)
	return q
}

// Enqueue admits sessionID for extraction. The bool says whether it was
// queued; the reason is one of eligible, ineligible:<gate>, duplicate,
// queue_full. Ownership is the caller's job (the HTTP handler checks it).
func (s *Service) Enqueue(sessionID string, mode Mode) (bool, string) {
	if s.queue == nil {
		return false, "queue_not_running"
	}
	return s.queue.Enqueue(sessionID, mode)
}

// Enqueue is Service.Enqueue on a running queue.
func (q *Queue) Enqueue(sessionID string, mode Mode) (bool, string) {
	if ok, reason := q.admissible(sessionID, mode); !ok {
		return false, reason
	}
	q.mu.Lock()
	if _, dup := q.pending[sessionID]; dup {
		q.mu.Unlock()
		return false, "duplicate"
	}
	select {
	case q.ch <- queueItem{sessionID: sessionID, mode: mode}:
		q.pending[sessionID] = struct{}{}
		q.mu.Unlock()
		return true, "eligible"
	default:
		q.mu.Unlock()
		slog.Warn("extraction queue full", "session", sessionID, "capacity", q.cfg.Capacity)
		return false, "queue_full"
	}
}

// admissible applies the gates for mode. Auto uses the full predicate;
// manual only refuses subagent sessions and unknown ones.
func (q *Queue) admissible(sessionID string, mode Mode) (bool, string) {
	now := float64(q.svc.now().UnixNano()) / 1e9
	switch mode {
	case ModeManual:
		st, err := q.svc.sessions.GetExtractionState(sessionID)
		if err != nil {
			return false, "ineligible:unknown_session"
		}
		if st.IsSubagent {
			return false, "ineligible:subagent"
		}
		return true, "eligible"
	default:
		ok, err := q.svc.sessions.IsEligibleForAutoExtraction(sessionID, now, q.cfg.Eligibility)
		if err != nil {
			slog.Error("eligibility check failed", "session", sessionID, "err", err)
			return false, "ineligible:error"
		}
		if !ok {
			return false, "ineligible:auto_gate"
		}
		return true, "eligible"
	}
}

// Len reports queued (not running) items, for stats and tests.
func (q *Queue) Len() int { return len(q.ch) }

func (q *Queue) worker(ctx context.Context, n int) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-q.ch:
			q.run(ctx, item)
		}
	}
}

func (q *Queue) run(ctx context.Context, item queueItem) {
	defer func() {
		q.mu.Lock()
		delete(q.pending, item.sessionID)
		q.mu.Unlock()
	}()
	// A session can stop being eligible while it waits (new ingest, another
	// worker finished it, gate changed): re-check before spending anything.
	if ok, reason := q.admissible(item.sessionID, item.mode); !ok {
		slog.Debug("extraction dequeued but no longer admissible",
			"session", item.sessionID, "mode", item.mode, "reason", reason)
		return
	}
	passCtx, cancel := context.WithTimeout(ctx, q.cfg.PassTimeout)
	defer cancel()
	res, err := q.svc.Extract(passCtx, item.sessionID)
	if err != nil {
		slog.Error("extraction failed", "session", item.sessionID, "mode", item.mode, "err", err)
		return
	}
	slog.Info("extraction pass complete",
		"session", item.sessionID, "mode", item.mode,
		"new", res.MessagesNew, "chunks", res.ChunksProcessed,
		"mems", res.MemoriesCreated, "errors", len(res.Errors),
		"through_id", res.ThroughID, "outcome", res.Outcome)
}
