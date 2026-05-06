package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/comma-compliance/arc-relay/internal/skills"
	"github.com/comma-compliance/arc-relay/internal/skills/checker"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// SkillsHandlers wraps skills.Service for HTTP. Like MemoryHandlers, it uses a
// closure to pull the authenticated user from context — keeps the package free
// of an import-cycle dependency on internal/server. UserStore is used only to
// resolve username → user_id for the assignment endpoints.
//
// checker is optional — when the cron is disabled (cfg.Skills.Checker.Enabled
// = false), main.go passes nil. handleCheckDrift returns 503 in that case so
// admins get a clear "feature is off" signal instead of a generic 500.
type SkillsHandlers struct {
	svc           *skills.Service
	store         *store.SkillStore
	users         *store.UserStore
	checker       *checker.Service
	userFromCtx   func(context.Context) *store.User
	apiKeyFromCtx func(context.Context) *store.APIKey
}

// NewSkillsHandlers creates SkillsHandlers wired to the skills service +
// stores. userFromCtx returns nil for unauth'd callers; handlers fail closed
// in that case. apiKeyFromCtx returns nil for session-cookie (web-login)
// auth; capability-gated paths fall back to user.Role == "admin" in that
// case (admins keep all powers without needing per-key capabilities). chk
// may be nil when the upstream-update checker is disabled; the on-demand
// check-drift endpoint reports 503 in that case.
func NewSkillsHandlers(
	svc *skills.Service,
	st *store.SkillStore,
	users *store.UserStore,
	chk *checker.Service,
	userFromCtx func(context.Context) *store.User,
	apiKeyFromCtx func(context.Context) *store.APIKey,
) *SkillsHandlers {
	return &SkillsHandlers{
		svc:           svc,
		store:         st,
		users:         users,
		checker:       chk,
		userFromCtx:   userFromCtx,
		apiKeyFromCtx: apiKeyFromCtx,
	}
}

// checkDriftRequestTimeout caps the wall-clock budget for a single on-demand
// check. Matches the cron's per-skill ceiling (4 × GitCloneTimeout default
// = 4 minutes is the cron path); we deliberately use a tighter HTTP-friendly
// 60s here so the user gets a 504-ish error fast on a slow clone instead of
// blocking the connection for minutes. Operators chasing genuinely-large
// repos can fall back to the cron run.
const checkDriftRequestTimeout = 60 * time.Second

// HandleSkills routes /api/skills. GET = list-for-user, POST not allowed
// (uploads are versioned and routed through HandleSkillByPath).
func (h *SkillsHandlers) HandleSkills(w http.ResponseWriter, r *http.Request) {
	user := h.userFromCtx(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	skillsList, err := h.listForUser(user)
	if err != nil {
		slog.Warn("skills list", "user", user.ID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Wrap each row so we can attach a per-skill drift block when present.
	// Embedding *store.Skill keeps the existing wire shape intact for callers
	// that ignore unknown fields; `omitempty` on Drift keeps non-outdated
	// rows byte-identical to the pre-Task-13 response.
	//
	// N+1 query trade-off: GetUpstream runs once per outdated skill. Acceptable
	// at admin scale (dozens of skills). If this ever lights up profiles, swap
	// in a batched ListUpstreamsByIDs.
	out := make([]skillResp, 0, len(skillsList))
	for _, sk := range skillsList {
		row := skillResp{Skill: sk}
		if sk.Outdated == 1 {
			if u, err := h.store.GetUpstream(sk.ID); err == nil {
				row.Drift = driftBlockFromUpstream(u)
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": out})
}

// skillResp is the per-row JSON shape for skill list/get endpoints. It embeds
// *store.Skill so the existing field set is emitted unchanged, and adds an
// optional drift block. `omitempty` ensures non-outdated rows match the pre-
// Task-13 wire format byte-for-byte.
type skillResp struct {
	*store.Skill
	Drift map[string]any `json:"drift,omitempty"`
}

// HandleAssigned returns the user's effective skill set: public + restricted-
// with-explicit-grant, plus version pin (if any). This is what `arc-sync skill
// sync` consumes to compute the desired client state.
func (h *SkillsHandlers) HandleAssigned(w http.ResponseWriter, r *http.Request) {
	user := h.userFromCtx(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rows, err := h.store.AssignedForUser(user.ID)
	if err != nil {
		slog.Warn("skills assigned", "user", user.ID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assigned": rows})
}

// HandleSkillByPath routes /api/skills/{slug}[/versions/{version}[/archive]].
// The leading prefix is stripped before this handler runs.
func (h *SkillsHandlers) HandleSkillByPath(w http.ResponseWriter, r *http.Request) {
	user := h.userFromCtx(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/skills/")
	parts := strings.Split(rest, "/")
	slug := parts[0]
	if slug == "" {
		writeJSONError(w, http.StatusBadRequest, "missing skill slug")
		return
	}
	skill, err := h.store.GetSkillBySlug(slug)
	if err != nil {
		slog.Warn("skills lookup", "slug", slug, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Discoverability: GET on a non-existent slug returns 404. Non-admin write
	// callers also see 404 (don't leak existence to non-admins).
	if skill == nil {
		// For uploads (POST) we let the slug be created on the fly — a 404 here
		// would block the natural "publish a brand new skill" flow.
		if !(r.Method == http.MethodPost && len(parts) >= 3 && parts[1] == "versions") {
			writeJSONError(w, http.StatusNotFound, "skill not found")
			return
		}
	}

	// Read-side ACL for non-admins: they can see public + their own assignments.
	if r.Method == http.MethodGet && skill != nil && user.Role != "admin" {
		if !h.userCanRead(user, skill) {
			writeJSONError(w, http.StatusNotFound, "skill not found")
			return
		}
	}

	switch len(parts) {
	case 1:
		// /api/skills/{slug}
		switch r.Method {
		case http.MethodGet:
			h.getSkill(w, skill)
		case http.MethodDelete:
			if user.Role != "admin" {
				writeJSONError(w, http.StatusForbidden, "admin access required")
				return
			}
			h.deleteSkill(w, r, skill)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case 2:
		// /api/skills/{slug}/versions   — list versions
		// /api/skills/{slug}/assignments — list assignments (admin) / POST grant (admin)
		// /api/skills/{slug}/check-drift — admin: trigger on-demand drift check
		switch parts[1] {
		case "versions":
			if r.Method != http.MethodGet {
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			h.listVersions(w, skill)
		case "assignments":
			if user.Role != "admin" {
				writeJSONError(w, http.StatusForbidden, "admin access required")
				return
			}
			switch r.Method {
			case http.MethodGet:
				h.listAssignments(w, skill)
			case http.MethodPost:
				h.assignSkill(w, r, skill, user.ID)
			default:
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		case "check-drift":
			if user.Role != "admin" {
				writeJSONError(w, http.StatusForbidden, "admin access required")
				return
			}
			h.handleCheckDrift(w, r, slug)
		case "upstream":
			// Admin OR API key with skills:write — same gate as version
			// uploads, since this is the same kind of write to the same table.
			if !requireCapability(w, r, user, h.apiKeyFromCtx(r.Context()), "skills:write") {
				return
			}
			h.handleUpstream(w, r, skill)
		default:
			writeJSONError(w, http.StatusNotFound, "unknown subresource")
		}
	case 3:
		// /api/skills/{slug}/versions/{version}
		// /api/skills/{slug}/assignments/{username} — DELETE only (admin)
		switch parts[1] {
		case "versions":
			version := parts[2]
			if version == "" {
				writeJSONError(w, http.StatusBadRequest, "missing version")
				return
			}
			switch r.Method {
			case http.MethodGet:
				h.getVersion(w, skill, version)
			case http.MethodPost:
				// Upload is gated by the `skills:write` capability so that
				// non-admin keys (e.g. CI server keys) can publish skills
				// without holding full admin powers. Admin keys bypass.
				if !requireCapability(w, r, user, h.apiKeyFromCtx(r.Context()), "skills:write") {
					return
				}
				h.uploadVersion(w, r, slug, version, user.ID)
			case http.MethodDelete:
				// Yank stays admin-only for now. Future enhancement: gate
				// behind `skills:yank` + an `uploaded_by_api_key_id` check
				// so a publisher can yank only their own versions. The
				// migration already added the column; the upload + yank
				// paths still need to read/write it.
				if user.Role != "admin" {
					writeJSONError(w, http.StatusForbidden, "admin access required")
					return
				}
				h.yankVersion(w, skill, version)
			default:
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		case "assignments":
			username := parts[2]
			if username == "" {
				writeJSONError(w, http.StatusBadRequest, "missing username")
				return
			}
			if user.Role != "admin" {
				writeJSONError(w, http.StatusForbidden, "admin access required")
				return
			}
			if r.Method != http.MethodDelete {
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			h.unassignSkill(w, skill, username)
		default:
			writeJSONError(w, http.StatusNotFound, "unknown subresource")
		}
	case 4:
		// /api/skills/{slug}/versions/{version}/archive
		if parts[1] != "versions" || parts[3] != "archive" {
			writeJSONError(w, http.StatusNotFound, "unknown subresource")
			return
		}
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.downloadArchive(w, r, skill, parts[2])
	default:
		writeJSONError(w, http.StatusNotFound, "unknown subresource")
	}
}

// listForUser returns skills visible to the user: admins see all; non-admins
// see public + their own assignments. Yanked skills are filtered for non-admins.
func (h *SkillsHandlers) listForUser(user *store.User) ([]*store.Skill, error) {
	if user.Role == "admin" {
		return h.store.ListSkills()
	}
	rows, err := h.store.AssignedForUser(user.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Skill, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Skill)
	}
	return out, nil
}

// userCanRead implements the visibility check used by single-skill GET endpoints.
// Mirrors AssignedForUser's WHERE clause: public skills are readable by all
// authenticated users; restricted skills require an explicit assignment.
// Yanked skills are hidden from non-admins.
func (h *SkillsHandlers) userCanRead(user *store.User, skill *store.Skill) bool {
	if user.Role == "admin" {
		return true
	}
	if skill.YankedAt != nil {
		return false
	}
	if skill.Visibility == "public" {
		return true
	}
	// Restricted — check assignment table.
	assigns, err := h.store.ListAssignmentsForSkill(skill.ID)
	if err != nil {
		return false
	}
	for _, a := range assigns {
		if a.UserID == user.ID {
			return true
		}
	}
	return false
}

func (h *SkillsHandlers) getSkill(w http.ResponseWriter, skill *store.Skill) {
	versions, err := h.store.ListVersions(skill.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	resp := map[string]any{
		"skill":    skill,
		"versions": versions,
	}
	// When the skill is flagged outdated, fetch the upstream row and surface
	// the drift block alongside the skill. Same wire-shape contract as the
	// list endpoint: missing/nil drift means the field is omitted entirely,
	// so existing consumers stay unaffected.
	if skill.Outdated == 1 {
		if u, err := h.store.GetUpstream(skill.ID); err == nil {
			if drift := driftBlockFromUpstream(u); drift != nil {
				resp["drift"] = drift
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *SkillsHandlers) listVersions(w http.ResponseWriter, skill *store.Skill) {
	versions, err := h.store.ListVersions(skill.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

func (h *SkillsHandlers) getVersion(w http.ResponseWriter, skill *store.Skill, version string) {
	v, err := h.store.GetVersion(skill.ID, version)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if v == nil {
		writeJSONError(w, http.StatusNotFound, "version not found")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// upstreamHeaderPayload is the JSON shape accepted in the X-Upstream header on
// version uploads. Mirrors store.SkillUpstream's identity fields. Type defaults
// to "git" when empty; Ref defaults to "HEAD" inside UpsertUpstream.
type upstreamHeaderPayload struct {
	Type    string `json:"type"`
	URL     string `json:"url"`
	Subpath string `json:"subpath"`
	Ref     string `json:"ref"`
}

func (h *SkillsHandlers) uploadVersion(w http.ResponseWriter, r *http.Request, slug, version, uploaderID string) {
	r.Body = http.MaxBytesReader(w, r.Body, skills.MaxArchiveSize)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "archive exceeds 5 MiB limit")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	visibility := r.URL.Query().Get("visibility")
	res, err := h.svc.Upload(&skills.UploadInput{
		SlugOverride: slug,
		Version:      version,
		Archive:      body,
		UploadedBy:   uploaderID,
		Visibility:   visibility,
	})
	if err != nil {
		switch {
		case errors.Is(err, skills.ErrInvalidArchive):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, store.ErrSkillVersionConflict):
			writeJSONError(w, http.StatusConflict, "version already exists")
		case errors.Is(err, store.ErrSkillSlugConflict):
			writeJSONError(w, http.StatusConflict, "slug already exists")
		default:
			slog.Warn("skills upload", "slug", slug, "version", version, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	// Optional upstream side-effects. Headers (not form fields) carry the
	// metadata so the wire format stays application/gzip — see Task 4 plan
	// notes. Failures here are logged but do not fail the upload; the version
	// row is already committed.
	upstreamRecorded := false
	clearUpstream := strings.EqualFold(r.Header.Get("X-Clear-Upstream"), "true")
	upstreamHeader := r.Header.Get("X-Upstream")
	switch {
	case clearUpstream:
		if err := h.store.ClearUpstream(res.Skill.ID); err != nil {
			slog.Warn("skills upload: clear upstream", "skill", res.Skill.ID, "err", err)
		}
	case upstreamHeader != "":
		var p upstreamHeaderPayload
		if err := json.Unmarshal([]byte(upstreamHeader), &p); err != nil {
			slog.Warn("skills upload: parse X-Upstream", "skill", res.Skill.ID, "err", err)
		} else if p.URL != "" && (p.Type == "" || p.Type == "git") {
			if err := h.store.UpsertUpstream(&store.SkillUpstream{
				SkillID:      res.Skill.ID,
				UpstreamType: p.Type,
				GitURL:       p.URL,
				GitSubpath:   p.Subpath,
				GitRef:       p.Ref,
			}); err != nil {
				slog.Warn("skills upload: upsert upstream", "skill", res.Skill.ID, "err", err)
			} else {
				upstreamRecorded = true
			}
		} else {
			slog.Warn("skills upload: X-Upstream rejected (need type=git + non-empty url)",
				"skill", res.Skill.ID, "type", p.Type, "url_empty", p.URL == "")
		}
	}

	// After ANY successful push, if an upstream row exists for this skill,
	// clear drift and re-baseline last_seen_hash to the just-uploaded
	// archive's subtree hash. The hash compute is non-fatal: a failure here
	// degrades to last_seen_hash="" (matches pre-Phase-4 behavior) but never
	// blocks the upload.
	relayHash, hashErr := h.svc.ComputeSubtreeHashFromArchive(body)
	if hashErr != nil {
		slog.Warn("skills upload: compute relay hash", "skill", res.Skill.ID, "err", hashErr)
		relayHash = ""
	}
	if upstream, err := h.store.GetUpstream(res.Skill.ID); err != nil {
		slog.Warn("skills upload: get upstream", "skill", res.Skill.ID, "err", err)
	} else if upstream != nil {
		if err := h.store.ClearDriftReport(res.Skill.ID, relayHash); err != nil {
			slog.Warn("skills upload: clear drift", "skill", res.Skill.ID, "err", err)
		}
	}

	writeJSON(w, http.StatusCreated, struct {
		*skills.UploadResult
		UpstreamRecorded bool `json:"upstream_recorded"`
	}{res, upstreamRecorded})
}

func (h *SkillsHandlers) deleteSkill(w http.ResponseWriter, r *http.Request, skill *store.Skill) {
	hard := r.URL.Query().Get("hard") == "true"
	if hard {
		if err := h.store.DeleteSkill(skill.ID); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.store.YankSkill(skill.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"yanked": true})
}

func (h *SkillsHandlers) yankVersion(w http.ResponseWriter, skill *store.Skill, version string) {
	if err := h.store.YankVersion(skill.ID, version); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"yanked": true})
}

// downloadArchive streams the archive bytes back to the client. We do not set
// Content-Length here; ServeContent would be wrong because we want the strong
// SHA-256 hash in headers and a binary download disposition.
func (h *SkillsHandlers) downloadArchive(w http.ResponseWriter, _ *http.Request, skill *store.Skill, version string) {
	rc, v, err := h.svc.OpenArchive(skill.ID, version)
	if err != nil {
		if errors.Is(err, skills.ErrSkillNotFound) {
			writeJSONError(w, http.StatusNotFound, "version not found")
			return
		}
		slog.Warn("skills download", "slug", skill.Slug, "version", version, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+skill.Slug+`-`+v.Version+`.tar.gz"`)
	w.Header().Set("X-Skill-SHA256", v.ArchiveSHA256)
	w.Header().Set("X-Skill-Version", v.Version)
	if _, err := io.Copy(w, rc); err != nil {
		// Already wrote headers — best we can do is stop streaming. Don't try
		// to write a JSON error body; that races the response writer.
		slog.Warn("skills download stream error", "slug", skill.Slug, "version", version, "err", err)
	}
}

// writeJSONError writes a {"error":msg} body with the given status. Reuses
// writeJSON from handlers.go.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// assignBody is the wire shape for POST /api/skills/{slug}/assignments and
// the analogous recipes endpoint. Username is resolved server-side to user_id;
// version is optional (NULL means "follow latest").
type assignBody struct {
	Username string `json:"username"`
	Version  string `json:"version,omitempty"`
}

// listAssignments returns the existing grants for a skill. Admin-only at the
// route level — all callers reaching here have already passed the admin check.
func (h *SkillsHandlers) listAssignments(w http.ResponseWriter, skill *store.Skill) {
	rows, err := h.store.ListAssignmentsForSkill(skill.ID)
	if err != nil {
		slog.Warn("skills list assignments", "slug", skill.Slug, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assignments": rows})
}

// assignSkill grants a user access to a restricted skill. Body shape:
//   {"username":"alice","version":"1.0.0"}
// version is optional. Idempotent: re-assigning replaces the prior pin.
func (h *SkillsHandlers) assignSkill(w http.ResponseWriter, r *http.Request, skill *store.Skill, adminID string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	var in assignBody
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(in.Username) == "" {
		writeJSONError(w, http.StatusBadRequest, "username is required")
		return
	}
	target, err := h.users.GetByUsername(in.Username)
	if err != nil {
		slog.Warn("skills assign user lookup", "username", in.Username, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if target == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	a := &store.SkillAssignment{
		SkillID: skill.ID,
		UserID:  target.ID,
	}
	if v := strings.TrimSpace(in.Version); v != "" {
		a.Version = &v
	}
	if adminID != "" {
		a.AssignedBy = &adminID
	}
	if err := h.store.AssignSkill(a); err != nil {
		slog.Warn("skills assign", "slug", skill.Slug, "user", in.Username, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// unassignSkill revokes a grant. The skill_id + username must both resolve
// for idempotency to be useful: an unassign on a non-existent user returns
// 404 so the caller knows the typo wasn't accepted as a no-op.
func (h *SkillsHandlers) unassignSkill(w http.ResponseWriter, skill *store.Skill, username string) {
	target, err := h.users.GetByUsername(username)
	if err != nil {
		slog.Warn("skills unassign user lookup", "username", username, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if target == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	if err := h.store.UnassignSkill(skill.ID, target.ID); err != nil {
		slog.Warn("skills unassign", "slug", skill.Slug, "user", username, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCheckDrift implements POST /api/skills/{slug}/check-drift.
// Admin-only (enforced by HandleSkillByPath before dispatching here).
//
// Status codes:
//   - 200 — drift detected (or already flagged); body is the drift block.
//   - 204 — up to date.
//   - 404 — slug unknown.
//   - 405 — non-POST.
//   - 409 — skill exists but has no skill_upstreams row.
//   - 502 — upstream fetch/clone failed.
//   - 503 — checker is disabled at startup (cron not configured).
//
// The 60s per-request timeout is independent of the cron's per-skill budget.
// Operators chasing genuinely-large repos can fall back to the cron run if
// the on-demand check times out.
func (h *SkillsHandlers) handleCheckDrift(w http.ResponseWriter, r *http.Request, slug string) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.checker == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "skill upstream checker disabled")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), checkDriftRequestTimeout)
	defer cancel()

	outcome, err := h.checker.RunOneSlug(ctx, slug)
	if err != nil {
		if errors.Is(err, checker.ErrSkillNotFound) {
			writeJSONError(w, http.StatusNotFound, "skill not found")
			return
		}
		slog.Warn("skills check-drift", "slug", slug, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	switch outcome {
	case checker.OutcomeNoUpstream:
		writeJSONError(w, http.StatusConflict, "skill has no upstream tracking enabled")
		return
	case checker.OutcomeFetchFailed:
		writeJSONError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	case checker.OutcomeUpToDate:
		w.WriteHeader(http.StatusNoContent)
		return
	case checker.OutcomeDrift:
		// Re-fetch the upstream row so we can return the just-persisted drift
		// block. A read failure here degrades to a 200 with no drift body
		// rather than masking the (already-committed) state change.
		skill, _ := h.store.GetSkillBySlug(slug)
		var upstream *store.SkillUpstream
		if skill != nil {
			upstream, _ = h.store.GetUpstream(skill.ID)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"outdated": true,
			"drift":    driftBlockFromUpstream(upstream),
		})
		return
	default:
		// Defensive: unknown outcome shouldn't reach here, but don't panic.
		slog.Warn("skills check-drift: unknown outcome", "slug", slug, "outcome", outcome)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

// upstreamPUTBody is the JSON shape accepted by PUT /api/skills/{slug}/upstream.
// Mirrors store.SkillUpstream's identity fields. Type defaults to "git" when
// empty; Ref defaults to "HEAD" inside UpsertUpstream.
type upstreamPUTBody struct {
	Type    string `json:"type"`
	GitURL  string `json:"git_url"`
	Subpath string `json:"git_subpath"`
	Ref     string `json:"git_ref"`
}

// handleUpstream implements PUT/DELETE /api/skills/{slug}/upstream.
//
// PUT replaces (or creates) the upstream-tracking row for an existing skill
// without requiring a version bump or a re-uploaded archive. DELETE removes
// the row and clears `skills.outdated` (atomic via SkillStore.ClearUpstream).
//
// Authorization is gated by the caller (admin OR API key with skills:write),
// so this method only enforces method shape, content type, and body validity.
//
// Status codes:
//
//	200 — upstream set/replaced (PUT) or removed (DELETE)
//	400 — body parse error, missing git_url, or unsupported type
//	405 — method not PUT/DELETE
//	415 — PUT without `Content-Type: application/json`
func (h *SkillsHandlers) handleUpstream(w http.ResponseWriter, r *http.Request, skill *store.Skill) {
	switch r.Method {
	case http.MethodPut:
		h.upsertUpstream(w, r, skill)
	case http.MethodDelete:
		if err := h.store.ClearUpstream(skill.ID); err != nil {
			slog.Warn("skills clear upstream", "skill", skill.ID, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// upsertUpstream is the PUT branch of handleUpstream.
func (h *SkillsHandlers) upsertUpstream(w http.ResponseWriter, r *http.Request, skill *store.Skill) {
	ct := r.Header.Get("Content-Type")
	// Accept "application/json" exactly or with parameters (e.g. "; charset=utf-8").
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if !strings.EqualFold(ct, "application/json") {
		writeJSONError(w, http.StatusUnsupportedMediaType, "expected application/json")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	var in upstreamPUTBody
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.Type != "" && in.Type != "git" {
		// Pre-validate the CHECK constraint so the response is 400 not 500.
		writeJSONError(w, http.StatusBadRequest, "unsupported upstream type (only \"git\" is supported)")
		return
	}
	if strings.TrimSpace(in.GitURL) == "" {
		writeJSONError(w, http.StatusBadRequest, "git_url is required")
		return
	}

	if err := h.store.UpsertUpstream(&store.SkillUpstream{
		SkillID:      skill.ID,
		UpstreamType: in.Type,
		GitURL:       in.GitURL,
		GitSubpath:   in.Subpath,
		GitRef:       in.Ref,
	}); err != nil {
		slog.Warn("skills upsert upstream", "skill", skill.ID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Re-fetch so the response includes server-side defaults (type, ref) and
	// any preserved last_seen_* / drift_* fields from a prior row.
	u, err := h.store.GetUpstream(skill.ID)
	if err != nil || u == nil {
		slog.Warn("skills upsert upstream: re-read", "skill", skill.ID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, upstreamResponse(u))
}

// upstreamResponse renders a SkillUpstream as the JSON shape the API returns
// from PUT /api/skills/{slug}/upstream and (in the future) a possible GET on
// the same path. Reuses driftBlockFromUpstream for the drift sub-block — null
// on a freshly-set row, populated once the next checker run records drift.
func upstreamResponse(u *store.SkillUpstream) map[string]any {
	out := map[string]any{
		"skill_id":      u.SkillID,
		"upstream_type": u.UpstreamType,
		"git_url":       u.GitURL,
		"git_subpath":   u.GitSubpath,
		"git_ref":       u.GitRef,
		"created_at":    u.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":    u.UpdatedAt.UTC().Format(time.RFC3339),
		"drift":         driftBlockFromUpstream(u),
	}
	if u.LastCheckedAt != nil {
		out["last_checked_at"] = u.LastCheckedAt.UTC().Format(time.RFC3339)
	} else {
		out["last_checked_at"] = nil
	}
	return out
}

// driftBlockFromUpstream formats the drift_* fields of a SkillUpstream into a
// JSON-friendly map. Returned shape mirrors the GET /api/skills/{slug}
// drift block that Task 13 will surface (and that the dashboard's drift
// banner will consume), so both endpoints can share a single helper.
//
// Returns nil when u is nil or has no drift recorded.
func driftBlockFromUpstream(u *store.SkillUpstream) map[string]any {
	if u == nil || u.DriftDetectedAt == nil {
		return nil
	}
	out := map[string]any{
		"detected_at": u.DriftDetectedAt.UTC().Format(time.RFC3339),
	}
	if u.DriftRelayVersion != nil {
		out["relay_version"] = *u.DriftRelayVersion
	}
	if u.DriftRelayHash != nil {
		out["relay_hash"] = *u.DriftRelayHash
	}
	if u.DriftUpstreamSHA != nil {
		out["upstream_sha"] = *u.DriftUpstreamSHA
	}
	if u.DriftUpstreamHash != nil {
		out["upstream_hash"] = *u.DriftUpstreamHash
	}
	if u.DriftCommitsAhead != nil {
		out["commits_ahead"] = *u.DriftCommitsAhead
	}
	if u.DriftSeverity != nil {
		out["severity"] = *u.DriftSeverity
	}
	if u.DriftSummary != nil {
		out["summary"] = *u.DriftSummary
	}
	if u.DriftRecommendedAction != nil {
		out["recommended_action"] = *u.DriftRecommendedAction
	}
	if u.DriftLLMModel != nil {
		out["llm_model"] = *u.DriftLLMModel
	}
	return out
}
