// Package web — memory dashboard handlers.
//
// All handlers in this file are read-only and require session-cookie auth.
// Per-user scoping comes from getUser(r).ID passed into memory.Service methods.
//
// The "## RESEARCH ONLY ..." banner is emitted by every transcript-rendering
// template directly (not via a Go-side wrapper) so the page contains the
// banner even on render errors.
package web

import (
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/comma-compliance/arc-relay/internal/store"
)

// pageSize is the standard page size for memory listings. Kept moderate so
// rendered pages stay token-efficient — the dashboard is meant to be browsed,
// not bulk-loaded into Claude's context.
const pageSize = 25

// HandleMemoryIndex renders /memory — the landing page. Tier 1 UX: project
// clustering instead of a flat 5-row teaser. Surfaces stats + recent
// projects with per-project session counts and last-active timestamps.
func (h *Handlers) HandleMemoryIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/memory" {
		http.NotFound(w, r)
		return
	}
	user := getUser(r)

	stats, err := h.memSvc.Stats()
	if err != nil {
		http.Error(w, "stats: "+err.Error(), http.StatusInternalServerError)
		return
	}
	projects, err := h.memSvc.RecentByProject(user.ID, 12)
	if err != nil {
		http.Error(w, "projects: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Decorate each project group with a display-friendly basename so the UI
	// doesn't have to do path-trimming in the template.
	type projectView struct {
		*store.ProjectGroup
		Basename string
	}
	views := make([]projectView, len(projects))
	for i, p := range projects {
		views[i] = projectView{ProjectGroup: p, Basename: filepath.Base(p.ProjectDir)}
	}

	// Pre-format stats counts to strings — the `commas` template func is
	// typed `func(int)`, but Stats fields are int64. Calling commas on
	// int64 silently aborts template rendering mid-page.
	statsView := map[string]any{
		"Sessions":     formatInt64(stats.Sessions),
		"Messages":     formatInt64(stats.Messages),
		"DBBytes":      formatBytes(stats.DBBytes),
		"LastIngestAt": stats.LastIngestAt,
		"Platforms":    stats.Platforms,
	}

	data := map[string]any{
		"Nav":      "memory",
		"User":     user,
		"Stats":    statsView,
		"Projects": views,
	}
	h.render(w, r, "memory.html", data)
}

// formatInt64 returns "1,234,567" for an int64. Avoids the mismatch with the
// existing `commas` template func, which only accepts int (32-bit on some
// platforms).
func formatInt64(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := false
	if len(s) > 0 && s[0] == '-' {
		neg = true
		s = s[1:]
	}
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	// Insert commas every 3 digits from the right.
	var out []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		// #nosec G115 -- s is strconv.FormatInt output, so every rune is ASCII
		// [-0-9] and the byte conversion cannot truncate.
		out = append(out, byte(c))
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// formatBytes renders a byte count as KiB/MiB/GiB to ~2 sig figs. Used for
// the DB-size stat on the memory landing page.
func formatBytes(n int64) string {
	const (
		KiB = 1 << 10
		MiB = 1 << 20
		GiB = 1 << 30
	)
	switch {
	case n >= GiB:
		return strconv.FormatFloat(float64(n)/GiB, 'f', 2, 64) + " GiB"
	case n >= MiB:
		return strconv.FormatFloat(float64(n)/MiB, 'f', 2, 64) + " MiB"
	case n >= KiB:
		return strconv.FormatFloat(float64(n)/KiB, 'f', 2, 64) + " KiB"
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}

// HandleMemorySessions renders /memory/sessions — paginated session list with
// optional project filter. Query params: ?page=N (1-indexed), ?project=DIR.
func (h *Handlers) HandleMemorySessions(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/memory/sessions" {
		http.NotFound(w, r)
		return
	}
	user := getUser(r)

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	projectDir := r.URL.Query().Get("project")
	offset := (page - 1) * pageSize

	sessions, total, err := h.memSvc.SessionsPaged(user.ID, projectDir, pageSize, offset)
	if err != nil {
		http.Error(w, "sessions: "+err.Error(), http.StatusInternalServerError)
		return
	}

	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}

	// Build query-string suffixes for pagination links so the project filter
	// survives next/prev clicks. Templates concatenate "?page=N" + this.
	filterQS := ""
	if projectDir != "" {
		filterQS = "&project=" + projectDir
	}

	data := map[string]any{
		"Nav":        "memory",
		"User":       user,
		"Sessions":   sessions,
		"ProjectDir": projectDir,
		"Page":       page,
		"TotalPages": totalPages,
		"TotalCount": total,
		"FilterQS":   filterQS,
		"PrevPage":   page - 1,
		"NextPage":   page + 1,
		"HasPrev":    page > 1,
		"HasNext":    page < totalPages,
	}
	h.render(w, r, "memory_sessions.html", data)
}

// HandleMemorySessionDetail renders /memory/sessions/{id} with a rich header
// (msg count, time span) and structured message rendering. Returns 404 for
// missing or other-user session IDs.
func (h *Handlers) HandleMemorySessionDetail(w http.ResponseWriter, r *http.Request) {
	user := getUser(r)
	sessionID := strings.TrimPrefix(r.URL.Path, "/memory/sessions/")
	if sessionID == "" {
		http.Redirect(w, r, "/memory/sessions", http.StatusFound)
		return
	}

	sess, msgs, err := h.memSvc.GetSessionWithMessages(user.ID, sessionID, 0)
	if err != nil {
		if strings.Contains(err.Error(), "session not found") {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Compute per-message display flags so the template doesn't need a
	// "long content" predicate. Long-content messages render inside a
	// <details> block to keep the page navigable.
	type messageView struct {
		*store.Message
		LongContent  bool
		ContentChars int
	}
	views := make([]messageView, len(msgs))
	totalChars := 0
	for i, m := range msgs {
		c := len(m.Content)
		totalChars += c
		views[i] = messageView{Message: m, LongContent: c > 2000, ContentChars: c}
	}

	data := map[string]any{
		"Nav":          "memory",
		"User":         user,
		"Session":      sess,
		"Messages":     views,
		"MessageCount": len(msgs),
		"TotalChars":   totalChars,
		"Basename":     filepath.Base(sess.ProjectDir),
	}
	h.render(w, r, "memory_session_detail.html", data)
}

// HandleMemorySearch renders /memory/search with date/project/role filters.
// Query params: ?q=text, ?project=DIR, ?role=user|assistant|tool, ?since_epoch=N.
// Empty q renders just the filter form.
func (h *Handlers) HandleMemorySearch(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/memory/search" {
		http.NotFound(w, r)
		return
	}
	user := getUser(r)
	q := r.URL.Query().Get("q")
	projectDir := r.URL.Query().Get("project")
	role := r.URL.Query().Get("role")
	sinceEpoch, _ := strconv.Atoi(r.URL.Query().Get("since_epoch"))

	// For the project dropdown — pull a project list so the user can scope
	// their search by recent project without retyping paths.
	projects, _ := h.memSvc.RecentByProject(user.ID, 30)

	data := map[string]any{
		"Nav":        "memory",
		"User":       user,
		"Query":      q,
		"ProjectDir": projectDir,
		"Role":       role,
		"SinceEpoch": sinceEpoch,
		"Projects":   projects,
		"Hits":       nil,
	}
	if q != "" {
		hits, err := h.memSvc.Search(user.ID, q, store.SearchOpts{
			Limit:      25,
			ProjectDir: projectDir,
			Role:       role,
			SinceEpoch: sinceEpoch,
		})
		if err != nil {
			http.Error(w, "search: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Group hits by session so the user sees clustered context rather
		// than 25 random snippets across 25 sessions.
		type group struct {
			SessionID string
			Hits      []*store.SearchHit
		}
		byID := map[string]*group{}
		var ordered []*group
		for _, hit := range hits {
			g, ok := byID[hit.SessionID]
			if !ok {
				g = &group{SessionID: hit.SessionID}
				byID[hit.SessionID] = g
				ordered = append(ordered, g)
			}
			g.Hits = append(g.Hits, hit)
		}
		data["Groups"] = ordered
		data["HitCount"] = len(hits)
	}
	h.render(w, r, "memory_search.html", data)
}
