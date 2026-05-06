// Package web — skill repository dashboard handlers.
//
// All handlers in this file require session-cookie auth (h.requireAuth wraps
// them at registration time). Read endpoints are scoped to what the user can
// see; write endpoints (upload, yank, delete) require admin role and verify
// the CSRF token from the form post.
package web

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/comma-compliance/arc-relay/internal/skills"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// skillUploadFormLimit caps multipart-parsed memory. The actual archive cap is
// enforced by skills.Service.Upload via skills.MaxArchiveSize. This number is
// just how much the multipart parser will hold in memory before spilling to
// /tmp.
const skillUploadFormLimit = 8 << 20 // 8 MiB

// HandleSkillsList renders /skills — the landing page listing all skills the
// user can see. Admins see the full catalog including yanked rows; non-admins
// see public + their explicit assignments (yanked rows hidden).
func (h *Handlers) HandleSkillsList(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/skills" {
		http.NotFound(w, r)
		return
	}
	user := getUser(r)

	var (
		list []*store.Skill
		err  error
	)
	if user.Role == "admin" {
		list, err = h.skillStore.ListSkills()
	} else {
		var assigned []*store.AssignedSkill
		assigned, err = h.skillStore.AssignedForUser(user.ID)
		if err == nil {
			list = make([]*store.Skill, 0, len(assigned))
			for _, a := range assigned {
				list = append(list, a.Skill)
			}
		}
	}
	if err != nil {
		slog.Warn("skills dashboard list", "user", user.ID, "err", err)
		http.Error(w, "failed to list skills", http.StatusInternalServerError)
		return
	}

	// Wrap each row so the template can render an "outdated · severity" pill
	// when the daily checker has flagged drift. Mirrors the JSON list endpoint
	// in skills_handlers.go (skillResp). N+1 GetUpstream is acceptable at
	// admin scale (dozens of skills).
	rows := make([]skillResp, 0, len(list))
	for _, sk := range list {
		row := skillResp{Skill: sk}
		if sk.Outdated == 1 {
			if u, err := h.skillStore.GetUpstream(sk.ID); err == nil {
				row.Drift = driftBlockFromUpstream(u)
			}
		}
		rows = append(rows, row)
	}

	h.render(w, r, "skills.html", map[string]any{
		"Nav":    "skills",
		"User":   user,
		"Skills": rows,
	})
}

// HandleSkillNew handles GET/POST /skills/new (upload form). Admin-only.
func (h *Handlers) HandleSkillNew(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.URL.Path != "/skills/new" {
		http.NotFound(w, r)
		return
	}
	user := getUser(r)

	if r.Method == http.MethodGet {
		h.render(w, r, "skill_new.html", map[string]any{
			"Nav":  "skills",
			"User": user,
		})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Cap the request body up-front so a hostile multipart POST can't allocate
	// gigabytes before we even reach the form parser. Cap matches the form
	// limit + a small headers slack.
	r.Body = http.MaxBytesReader(w, r.Body, skills.MaxArchiveSize+1<<16)

	if err := r.ParseMultipartForm(skillUploadFormLimit); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.renderSkillNewWithError(w, r, user, "Archive exceeds 5 MiB upload limit.")
			return
		}
		h.renderSkillNewWithError(w, r, user, "Invalid multipart form: "+err.Error())
		return
	}
	if !h.validateCSRF(r, getSessionID(r.Context())) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	slug := strings.TrimSpace(r.FormValue("slug"))
	version := strings.TrimSpace(r.FormValue("version"))
	visibility := r.FormValue("visibility")
	if slug == "" || version == "" {
		h.renderSkillNewWithError(w, r, user, "Slug and version are required.")
		return
	}

	file, _, err := r.FormFile("archive")
	if err != nil {
		h.renderSkillNewWithError(w, r, user, "Upload an archive (.tar.gz) for this skill.")
		return
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, skills.MaxArchiveSize+1))
	if err != nil {
		h.renderSkillNewWithError(w, r, user, "Could not read uploaded archive.")
		return
	}
	if int64(len(body)) > skills.MaxArchiveSize {
		h.renderSkillNewWithError(w, r, user, "Archive exceeds 5 MiB upload limit.")
		return
	}

	res, err := h.skillSvc.Upload(&skills.UploadInput{
		SlugOverride: slug,
		Version:      version,
		Archive:      body,
		UploadedBy:   user.ID,
		Visibility:   visibility,
	})
	if err != nil {
		switch {
		case errors.Is(err, skills.ErrInvalidArchive):
			h.renderSkillNewWithError(w, r, user, err.Error())
		case errors.Is(err, store.ErrSkillVersionConflict):
			h.renderSkillNewWithError(w, r, user, fmt.Sprintf("Version %s already exists for skill %q.", version, slug))
		case errors.Is(err, store.ErrSkillSlugConflict):
			h.renderSkillNewWithError(w, r, user, fmt.Sprintf("Slug %q is already in use.", slug))
		default:
			slog.Warn("skills dashboard upload", "slug", slug, "version", version, "err", err)
			h.renderSkillNewWithError(w, r, user, "Internal error while uploading.")
		}
		return
	}
	http.Redirect(w, r, "/skills/"+res.Skill.Slug, http.StatusFound)
}

// renderSkillNewWithError renders the upload form back with the error message
// and the user's last form values prefilled. r.Form is populated by
// ParseMultipartForm so the prefill works even on the failure path.
func (h *Handlers) renderSkillNewWithError(w http.ResponseWriter, r *http.Request, user *store.User, msg string) {
	h.render(w, r, "skill_new.html", map[string]any{
		"Nav":   "skills",
		"User":  user,
		"Error": msg,
		"Form": map[string]string{
			"Slug":       r.FormValue("slug"),
			"Version":    r.FormValue("version"),
			"Visibility": r.FormValue("visibility"),
		},
	})
}

// HandleSkillRoutes routes /skills/{slug}[/action]. GET on a bare slug renders
// the detail page; POSTs implement the yank/unyank/delete actions.
func (h *Handlers) HandleSkillRoutes(w http.ResponseWriter, r *http.Request) {
	user := getUser(r)
	rest := strings.TrimPrefix(r.URL.Path, "/skills/")
	if rest == "" {
		http.Redirect(w, r, "/skills", http.StatusFound)
		return
	}
	parts := strings.Split(rest, "/")
	slug := parts[0]

	skill, err := h.skillStore.GetSkillBySlug(slug)
	if err != nil {
		slog.Warn("skill dashboard lookup", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if skill == nil {
		http.NotFound(w, r)
		return
	}
	if user.Role != "admin" && !h.userCanReadSkill(user, skill) {
		http.NotFound(w, r)
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			h.renderSkillDetail(w, r, user, skill)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// Mutating actions: admin-only, CSRF-checked, POST-only.
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if !h.validateCSRF(r, getSessionID(r.Context())) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	switch parts[1] {
	case "yank":
		if err := h.skillStore.YankSkill(skill.ID); err != nil {
			slog.Warn("yank skill", "slug", slug, "err", err)
			http.Error(w, "yank failed", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/skills/"+slug, http.StatusFound)
	case "unyank":
		if err := h.skillStore.UnyankSkill(skill.ID); err != nil {
			slog.Warn("unyank skill", "slug", slug, "err", err)
			http.Error(w, "unyank failed", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/skills/"+slug, http.StatusFound)
	case "delete":
		if err := h.skillStore.DeleteSkill(skill.ID); err != nil {
			slog.Warn("delete skill", "slug", slug, "err", err)
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/skills", http.StatusFound)
	case "upstream":
		// /skills/{slug}/upstream        — POST sets/replaces the upstream row
		// /skills/{slug}/upstream/clear  — POST removes it
		if len(parts) == 2 {
			h.handleUpstreamForm(w, r, skill)
			return
		}
		if len(parts) == 3 && parts[2] == "clear" {
			if err := h.skillStore.ClearUpstream(skill.ID); err != nil {
				slog.Warn("clear upstream", "slug", slug, "err", err)
				http.Error(w, "clear upstream failed", http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, "/skills/"+slug, http.StatusFound)
			return
		}
		http.NotFound(w, r)
	case "versions":
		if len(parts) < 4 {
			http.NotFound(w, r)
			return
		}
		version := parts[2]
		switch parts[3] {
		case "yank":
			if err := h.skillStore.YankVersion(skill.ID, version); err != nil {
				slog.Warn("yank version", "slug", slug, "version", version, "err", err)
				http.Error(w, "yank failed", http.StatusInternalServerError)
				return
			}
		case "unyank":
			if err := h.skillStore.UnyankVersion(skill.ID, version); err != nil {
				slog.Warn("unyank version", "slug", slug, "version", version, "err", err)
				http.Error(w, "unyank failed", http.StatusInternalServerError)
				return
			}
		default:
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/skills/"+slug, http.StatusFound)
	default:
		http.NotFound(w, r)
	}
}

// renderSkillDetail builds the data context for /skills/{slug}.
func (h *Handlers) renderSkillDetail(w http.ResponseWriter, r *http.Request, user *store.User, skill *store.Skill) {
	versions, err := h.skillStore.ListVersions(skill.ID)
	if err != nil {
		slog.Warn("skill detail versions", "slug", skill.Slug, "err", err)
		http.Error(w, "failed to load versions", http.StatusInternalServerError)
		return
	}
	var assignments []*store.SkillAssignment
	if user.Role == "admin" {
		assignments, err = h.skillStore.ListAssignmentsForSkill(skill.ID)
		if err != nil {
			slog.Warn("skill detail assignments", "slug", skill.Slug, "err", err)
			http.Error(w, "failed to load assignments", http.StatusInternalServerError)
			return
		}
	}

	// Surface upstream tracking + drift state. Upstream may be nil (skill
	// pushed with --no-upstream); drift is only non-nil when the cron has
	// flagged this skill outdated.
	var (
		upstream *store.SkillUpstream
		drift    map[string]any
	)
	if u, err := h.skillStore.GetUpstream(skill.ID); err == nil && u != nil {
		upstream = u
		if skill.Outdated == 1 {
			drift = driftBlockFromUpstream(u)
		}
	}

	h.render(w, r, "skill_detail.html", map[string]any{
		"Nav":          "skills",
		"User":         user,
		"Skill":        skill,
		"Versions":     versions,
		"Assignments":  assignments,
		"DownloadBase": "/api/skills/" + skill.Slug + "/versions",
		"Upstream":     upstream,
		"Drift":        drift,
		"UpstreamForm": map[string]string{},
	})
}

// handleUpstreamForm processes the /skills/{slug}/upstream POST that the
// admin "Update tracking" card emits. The form's hidden CSRF token has
// already been validated and admin role enforced by HandleSkillRoutes
// before this is called.
//
// Validation is deliberately tight — git_url is required and non-empty, and
// the type is hardcoded to "git" (the only value the schema accepts). On
// validation failure we re-render the detail page with an inline error so
// the admin doesn't lose their typed values to a redirect.
func (h *Handlers) handleUpstreamForm(w http.ResponseWriter, r *http.Request, skill *store.Skill) {
	user := getUser(r)
	gitURL := strings.TrimSpace(r.FormValue("git_url"))
	subpath := strings.TrimSpace(r.FormValue("git_subpath"))
	ref := strings.TrimSpace(r.FormValue("git_ref"))

	if gitURL == "" {
		h.renderSkillDetailWithUpstreamError(w, r, user, skill, "Upstream git URL is required.", gitURL, subpath, ref)
		return
	}

	if err := h.skillStore.UpsertUpstream(&store.SkillUpstream{
		SkillID:    skill.ID,
		GitURL:     gitURL,
		GitSubpath: subpath,
		GitRef:     ref,
	}); err != nil {
		slog.Warn("upsert upstream", "slug", skill.Slug, "err", err)
		h.renderSkillDetailWithUpstreamError(w, r, user, skill, "Saving upstream tracking failed: "+err.Error(), gitURL, subpath, ref)
		return
	}
	http.Redirect(w, r, "/skills/"+skill.Slug, http.StatusFound)
}

// renderSkillDetailWithUpstreamError re-renders /skills/{slug} with an
// upstream-specific error and the user's last form values so they can fix
// the input without retyping.
func (h *Handlers) renderSkillDetailWithUpstreamError(w http.ResponseWriter, r *http.Request, user *store.User, skill *store.Skill, msg, gitURL, subpath, ref string) {
	versions, err := h.skillStore.ListVersions(skill.ID)
	if err != nil {
		slog.Warn("skill detail versions", "slug", skill.Slug, "err", err)
		http.Error(w, "failed to load versions", http.StatusInternalServerError)
		return
	}
	var assignments []*store.SkillAssignment
	if user.Role == "admin" {
		assignments, err = h.skillStore.ListAssignmentsForSkill(skill.ID)
		if err != nil {
			slog.Warn("skill detail assignments", "slug", skill.Slug, "err", err)
			http.Error(w, "failed to load assignments", http.StatusInternalServerError)
			return
		}
	}
	var (
		upstream *store.SkillUpstream
		drift    map[string]any
	)
	if u, err := h.skillStore.GetUpstream(skill.ID); err == nil && u != nil {
		upstream = u
		if skill.Outdated == 1 {
			drift = driftBlockFromUpstream(u)
		}
	}
	h.render(w, r, "skill_detail.html", map[string]any{
		"Nav":          "skills",
		"User":         user,
		"Skill":        skill,
		"Versions":     versions,
		"Assignments":  assignments,
		"DownloadBase": "/api/skills/" + skill.Slug + "/versions",
		"Upstream":     upstream,
		"Drift":        drift,
		"UpstreamErr":  msg,
		"UpstreamForm": map[string]string{
			"GitURL":  gitURL,
			"Subpath": subpath,
			"Ref":     ref,
		},
	})
}

// userCanReadSkill mirrors the visibility check used by the REST handlers, so
// the dashboard and the API agree on which skills a non-admin can see.
// Yanked skills are hidden from non-admins.
func (h *Handlers) userCanReadSkill(user *store.User, skill *store.Skill) bool {
	if skill.YankedAt != nil {
		return false
	}
	if skill.Visibility == "public" {
		return true
	}
	assigns, err := h.skillStore.ListAssignmentsForSkill(skill.ID)
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
