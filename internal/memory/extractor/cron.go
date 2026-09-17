package extractor

import (
	"context"
	"log/slog"
	"time"
)

// RunCron is the periodic backstop. Every `interval` it enqueues up to
// cronBatchSize sessions that pass the automatic eligibility gate, least
// recently attempted first, and prunes old failure rows. It never extracts
// directly: the queue is the only path, so the worker pool and the call-rate
// limit bound cron and watcher-triggered work together.
//
// Returns when ctx is canceled.
func (s *Service) RunCron(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	slog.Info("extractor cron loop started", "interval", interval)
	for {
		select {
		case <-ctx.Done():
			slog.Info("extractor cron loop stopped")
			return
		case <-t.C:
			s.cronCycle()
		}
	}
}

const (
	cronBatchSize     = 50
	errorRowRetention = 30 * 24 * time.Hour
)

// cronCycle enqueues one batch of eligible sessions. Without a running queue
// (the service is configured but StartQueue was never called) it logs and
// does nothing rather than extracting outside the admission path.
func (s *Service) cronCycle() {
	if s.queue == nil {
		slog.Warn("cron: extraction queue not running; skipping cycle")
		return
	}
	start := s.now()
	now := float64(start.UnixNano()) / 1e9
	sessions, err := s.sessions.ListEligibleForAutoExtraction(now, s.queue.cfg.Eligibility, cronBatchSize)
	if err != nil {
		slog.Error("cron: list eligible failed", "err", err)
		return
	}

	var queued, skipped int
	for _, sid := range sessions {
		if ok, _ := s.queue.Enqueue(sid, ModeAuto); ok {
			queued++
		} else {
			skipped++
		}
	}

	pruned, err := s.extractions.PruneErrors(now - errorRowRetention.Seconds())
	if err != nil {
		slog.Warn("cron: prune error rows failed", "err", err)
	}

	slog.Info("cron cycle complete",
		"eligible", len(sessions), "queued", queued, "skipped", skipped,
		"pruned_error_rows", pruned,
		"ms", time.Since(start).Milliseconds())
}
