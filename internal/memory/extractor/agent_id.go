package extractor

import (
	"path/filepath"
	"strings"
)

// agentIDPrefix is the namespace prefix for every transcript-derived memory
// in mem0. Any agent_id matching `transcripts-*` is owned by this pipeline;
// non-transcript memories (manual /code-memory writes from inside Claude
// sessions) live under different agent_ids and never blend with these.
const agentIDPrefix = "transcripts-"

// Derive normalizes a project directory to a stable mem0 agent_id.
//
//	/Users/ian/code/arc-relay        -> transcripts-arc-relay
//	/Users/ian/.claude               -> transcripts-claude
//	/home/foo/My Stuff               -> transcripts-my-stuff
//	/Users/ian/code/arc-relay/       -> transcripts-arc-relay
//	""                                -> transcripts-unknown
//
// Basename collisions across different machines/clones are disambiguated
// per-memory via metadata.project_dir at search time, not here.
func Derive(projectDir string) string {
	base := filepath.Base(strings.TrimSpace(projectDir))
	// filepath.Base returns "." for empty/just-whitespace input, "/" for "/", etc.
	switch base {
	case "", ".", "/":
		return agentIDPrefix + "unknown"
	}

	base = strings.ToLower(base)

	var b strings.Builder
	b.Grow(len(base))
	prevDash := false
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "unknown"
	}
	return agentIDPrefix + s
}

// IsTranscriptAgentID returns true if id looks like one we produced. Used by
// the blended /recall path to filter mem0 results to just transcript-derived
// memories.
func IsTranscriptAgentID(id string) bool {
	return strings.HasPrefix(id, agentIDPrefix)
}
