package extractor

import (
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/store"
)

func TestKeepMessage(t *testing.T) {
	cases := []struct {
		name string
		role string
		body string
		keep bool
	}{
		// Tier 1
		{"tool role dropped", "tool", "/Users/ian/code/arc-relay/foo.go: 200 lines of source", false},
		{"system role dropped", "system", "Session started at 2026-04-28T19:59:00Z with banner", false},

		// Tier 2
		{"empty user dropped", "user", "", false},
		{"short user dropped", "user", "yes", false},
		{"short user with whitespace dropped", "user", "  ok    ", false},
		{"19 rune user dropped", "user", strings.Repeat("a", 19), false},
		{"20 rune user kept", "user", strings.Repeat("a", 20), true},

		// Tier 3 — envelopes
		{"json object dropped", "assistant", `{"foo":"bar","baz":[1,2,3]}`, false},
		{"json array dropped", "assistant", `[{"a":1},{"b":2}]`, false},
		{"non-parseable braces kept", "assistant", `{this is just prose with curly braces}`, true},
		{"single bash line dropped", "assistant", "$ git status --short", false},
		{"single tee line dropped", "assistant", "> ls -la /etc/komodo", false},
		{"fenced bash block dropped", "assistant", "```bash\ngit status\necho hello\n```", false},
		{"fenced bash block with prose kept", "assistant", "Here's how to check status:\n```bash\ngit status\n```", true},

		// Real-world signal messages — must be kept
		{"feedback correction kept", "user", "stop summarizing what you just did at the end of every response", true},
		{"project decision kept", "assistant", "We're locking on Path A — mem0 does the LLM extraction with custom_instructions", true},
		{"reference tip kept", "user", "the Linear project INGEST is where we track all pipeline bugs", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &store.Message{Role: c.role, Content: c.body}
			got := KeepMessage(m)
			if got != c.keep {
				t.Errorf("KeepMessage role=%q body=%q: got %v want %v",
					c.role, truncate(c.body, 60), got, c.keep)
			}
		})
	}
}

func TestFilter_StatsAccounting(t *testing.T) {
	msgs := []*store.Message{
		{Role: "tool", Content: "anything"},                                               // tier 1
		{Role: "system", Content: "anything"},                                             // tier 1
		{Role: "user", Content: "ok"},                                                     // tier 2
		{Role: "assistant", Content: `{"action":"run","cmd":"git status"}`},               // tier 3
		{Role: "user", Content: "this is a substantive request to do work"},               // kept
		{Role: "assistant", Content: "and this is a substantive response with reasoning"}, // kept
	}
	kept, stats := Filter(msgs)
	if len(kept) != 2 {
		t.Errorf("kept len: got %d want 2", len(kept))
	}
	if stats.Total != 6 || stats.KeptCount != 2 || stats.ToolCount != 2 ||
		stats.ShortCount != 1 || stats.EnvelopeCount != 1 {
		t.Errorf("stats accounting wrong: %+v", stats)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
