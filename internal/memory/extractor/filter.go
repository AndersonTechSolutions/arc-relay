// Package extractor implements the LLM-driven extraction pipeline that
// distills transcript messages into structured mem0 memories.
//
// The package owns three concerns:
//  1. Pre-extraction filter (filter.go) — drops noise messages before they
//     ever reach the LLM. Pure rule-based, no LLM cost.
//  2. Chunking (chunk.go) — groups filtered messages into ~5K-char windows
//     suitable for one mem0.add_memory call.
//  3. Service (extractor.go) — orchestrates filter → chunk → mem0 → log,
//     with idempotency guards and per-session mutexes.
//
// The LLM extraction itself is delegated to mem0 (steered via the
// custom_instructions in prompts.go). We never call an LLM directly from
// this package — mem0 is the only LLM consumer.
package extractor

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/comma-compliance/arc-relay/internal/store"
)

// KeepMessage decides if a single message survives the pre-extraction filter.
// Three tiers, applied in order — the first failure short-circuits.
//
// Tier 1: drop tool/system messages outright. Tool-role rows are file reads,
// command outputs, MCP envelopes — almost no semantic content per token.
// System rows are framework banners.
//
// Tier 2: drop short messages (<20 runes after trimming). These are
// acknowledgements ("ok", "yes go ahead", "looks good") with no content
// worth memorizing.
//
// Tier 3: drop messages that are 100% bash command, 100% JSON envelope, or
// a single fenced code block with no surrounding prose. These are typically
// tool-call payloads that leaked through as assistant role.
//
// Conservative when in doubt — keeping a noise message wastes a few tokens,
// dropping a signal message loses information forever.
func KeepMessage(m *store.Message) bool {
	// Tier 1
	switch m.Role {
	case "tool", "system":
		return false
	}

	// Tier 2
	trimmed := strings.TrimSpace(m.Content)
	if utf8.RuneCountInString(trimmed) < 20 {
		return false
	}

	// Tier 3
	return !isEnvelope(trimmed)
}

// isEnvelope returns true if the entire message is a single bash command,
// a single parseable JSON object/array, or a single fenced code block with
// no surrounding prose.
func isEnvelope(s string) bool {
	// Pure JSON object or array
	if (strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) ||
		(strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]")) {
		var v any
		if json.Unmarshal([]byte(s), &v) == nil {
			return true
		}
	}

	// Single fenced code block with nothing outside the fence
	if strings.HasPrefix(s, "```") {
		// Look for closing fence
		end := strings.LastIndex(s, "```")
		// end must be after the opening fence and on its own line-ish
		if end > 3 {
			tail := strings.TrimSpace(s[end+3:])
			head := s[:end]
			// Make sure the only ``` are the opening and closing
			openLine := strings.Index(head, "\n")
			if openLine == -1 {
				return false
			}
			body := head[openLine+1:]
			// Check there's no prose between opening fence and a ```
			if tail == "" && !strings.Contains(body, "```") {
				return true
			}
		}
	}

	// Single shell line ($ or > prefix, no blank lines)
	if (strings.HasPrefix(s, "$ ") || strings.HasPrefix(s, "> ")) &&
		!strings.Contains(s, "\n\n") {
		return true
	}

	return false
}

// FilterStats summarizes a filter pass. Returned by Filter() for diagnostics
// and dry-run output.
type FilterStats struct {
	Total         int
	KeptCount     int
	ToolCount     int // tier 1 drops
	ShortCount    int // tier 2 drops
	EnvelopeCount int // tier 3 drops
}

// Filter applies KeepMessage to a slice and returns the survivors plus
// per-tier drop counts for diagnostics.
func Filter(msgs []*store.Message) ([]*store.Message, FilterStats) {
	stats := FilterStats{Total: len(msgs)}
	out := make([]*store.Message, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case m.Role == "tool" || m.Role == "system":
			stats.ToolCount++
			continue
		}
		trimmed := strings.TrimSpace(m.Content)
		if utf8.RuneCountInString(trimmed) < 20 {
			stats.ShortCount++
			continue
		}
		if isEnvelope(trimmed) {
			stats.EnvelopeCount++
			continue
		}
		out = append(out, m)
		stats.KeptCount++
	}
	return out, stats
}
