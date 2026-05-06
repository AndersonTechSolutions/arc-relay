package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Skill mirrors the relay's store.Skill JSON shape. Defined here (vs imported
// from internal/store) so arc-sync stays a pure-Go binary with no CGO/sqlite
// linkage. Wire shape kept in sync by hand.
type Skill struct {
	ID            string     `json:"id"`
	Slug          string     `json:"slug"`
	DisplayName   string     `json:"display_name"`
	Description   string     `json:"description"`
	Visibility    string     `json:"visibility"`
	LatestVersion string     `json:"latest_version,omitempty"`
	YankedAt      *time.Time `json:"yanked_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	// Outdated is 1 when the relay has detected drift against the recorded
	// upstream (Task 3 of the skill update checker plan). Omitted when the
	// flag isn't set or the skill has no upstream tracking.
	Outdated int `json:"outdated,omitempty"`
	// Drift carries the most recent drift report (Task 13). Present only when
	// Outdated == 1; the relay attaches it on list/detail responses so callers
	// can render severity without a second round-trip.
	Drift *DriftBlock `json:"drift,omitempty"`
}

// DriftBlock is the JSON-tagged drift report emitted by the relay's drift
// checker (Task 12 / Task 13 of the skill update checker plan). It mirrors
// the helper output and the per-skill `drift` JSON field.
type DriftBlock struct {
	Severity          string    `json:"severity"`
	Summary           string    `json:"summary"`
	RecommendedAction string    `json:"recommended_action"`
	CommitsAhead      int       `json:"commits_ahead"`
	UpstreamSHA       string    `json:"upstream_sha"`
	DetectedAt        time.Time `json:"detected_at"`
	RelayVersion      string    `json:"relay_version"`
	RelayHash         string    `json:"relay_hash"`
	LLMModel          string    `json:"llm_model"`
}

// SkillVersion mirrors the relay's store.SkillVersion JSON shape.
type SkillVersion struct {
	SkillID       string          `json:"skill_id"`
	Version       string          `json:"version"`
	ArchivePath   string          `json:"archive_path"`
	ArchiveSize   int64           `json:"archive_size"`
	ArchiveSHA256 string          `json:"archive_sha256"`
	Manifest      json.RawMessage `json:"manifest"`
	YankedAt      *time.Time      `json:"yanked_at,omitempty"`
	UploadedAt    time.Time       `json:"uploaded_at"`
}

// AssignedSkill is the row shape from GET /api/skills/assigned.
type AssignedSkill struct {
	Skill         *Skill  `json:"skill"`
	PinnedVersion *string `json:"pinned_version,omitempty"`
}

// SkillDetail is the response from GET /api/skills/{slug}.
type SkillDetail struct {
	Skill    *Skill          `json:"skill"`
	Versions []*SkillVersion `json:"versions"`
}

// UploadSkillResult is the response from POST /api/skills/{slug}/versions/{version}.
type UploadSkillResult struct {
	Skill   *Skill        `json:"skill"`
	Version *SkillVersion `json:"version"`
}

// UpstreamMetadata is the JSON shape sent in the `X-Upstream` header on a
// version upload (see Task 4: internal/web/skills_handlers.go's
// upstreamHeaderPayload — the shapes must match).
//
// Sentinel: empty Type AND empty URL means "clear the recorded upstream".
// UploadSkill consults this to decide between `X-Upstream: <json>` and
// `X-Clear-Upstream: true`. Defined in this package (rather than the higher-
// level sync package) so UploadSkill can take it as a parameter without
// creating a cycle — sync imports relay today.
type UpstreamMetadata struct {
	Type    string `json:"type"`
	URL     string `json:"url"`
	Subpath string `json:"subpath"`
	Ref     string `json:"ref"`
}

// ListSkills calls GET /api/skills. Returns whatever the user can see — admin
// gets the full catalog (incl. yanked); non-admin gets public + assigned.
func (c *Client) ListSkills() ([]*Skill, error) {
	body, err := c.skillGet("/api/skills")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Skills []*Skill `json:"skills"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse skills list: %w", err)
	}
	return resp.Skills, nil
}

// ListAssignedSkills calls GET /api/skills/assigned. Used by `arc-sync skill
// sync` to compute the desired client state.
func (c *Client) ListAssignedSkills() ([]*AssignedSkill, error) {
	body, err := c.skillGet("/api/skills/assigned")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Assigned []*AssignedSkill `json:"assigned"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse assigned skills: %w", err)
	}
	return resp.Assigned, nil
}

// GetSkill calls GET /api/skills/{slug} and returns the metadata + version list.
// Returns nil with no error if the skill doesn't exist or the user can't see it
// (HTTP 404).
func (c *Client) GetSkill(slug string) (*SkillDetail, error) {
	body, err := c.skillGet("/api/skills/" + url.PathEscape(slug))
	if err != nil {
		if e, ok := err.(*SkillHTTPError); ok && e.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	var resp SkillDetail
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse skill detail: %w", err)
	}
	return &resp, nil
}

// CheckDrift calls POST /api/skills/{slug}/check-drift (Task 12). The relay
// fetches the recorded upstream, compares it against the latest stored
// version, and either records new drift or reports the current state.
//
// Returns:
//   - (drift, nil) on 200 — drift detected; DriftBlock has the LLM-classified
//     severity/summary/recommended_action.
//   - (nil, nil) on 204 — up-to-date with upstream.
//   - (nil, *SkillHTTPError) on 404 (skill not found), 409 (no upstream
//     configured), 502 (upstream fetch failed), or other non-2xx. Callers
//     can `errors.As` to inspect Status and pretty-print.
func (c *Client) CheckDrift(slug string) (*DriftBlock, error) {
	endpoint := "/api/skills/" + url.PathEscape(slug) + "/check-drift"
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		var wrap struct {
			Outdated bool        `json:"outdated"`
			Drift    *DriftBlock `json:"drift"`
		}
		if err := json.Unmarshal(body, &wrap); err != nil {
			return nil, fmt.Errorf("parsing drift response: %w", err)
		}
		return wrap.Drift, nil
	}
	// Wrap in SkillHTTPError so callers can distinguish 404/409/502 via
	// errors.As — handleErrorResponse alone returns a plain error.
	return nil, &SkillHTTPError{
		Status: resp.StatusCode,
		err:    handleErrorResponse(resp, body, fmt.Sprintf("skill %q check-drift", slug)),
	}
}

// DownloadSkillVersion fetches the archive bytes for (slug, version). Returns
// the body, the SHA-256 from the X-Skill-SHA256 response header (used by the
// caller to verify integrity post-download), and the size in bytes.
func (c *Client) DownloadSkillVersion(slug, version string) (archive []byte, sha256 string, err error) {
	endpoint := fmt.Sprintf("/api/skills/%s/versions/%s/archive",
		url.PathEscape(slug), url.PathEscape(version))
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+endpoint, nil)
	if err != nil {
		return nil, "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("reading archive: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", handleErrorResponse(resp, body, fmt.Sprintf("skill %q@%s", slug, version))
	}
	return body, resp.Header.Get("X-Skill-SHA256"), nil
}

// UploadSkill posts an archive to POST /api/skills/{slug}/versions/{version}.
// Body is the raw .tar.gz bytes. Visibility is one of "public", "restricted",
// or "" (server default = "restricted" on first publish; ignored on
// re-publish).
//
// upstream carries optional upstream-tracking metadata (see Task 6 of the
// skill update checker plan). It maps to the relay's two-header protocol:
//   - upstream == nil: send neither header (no upstream change requested).
//   - upstream != nil with empty Type AND empty URL (the clear sentinel): send
//     `X-Clear-Upstream: true` to disassociate the skill from any prior upstream.
//   - upstream != nil with non-empty URL: marshal to JSON and send as the
//     `X-Upstream` header. Empty Type defaults to "git" server-side.
func (c *Client) UploadSkill(slug, version, visibility string, archive []byte, upstream *UpstreamMetadata) (*UploadSkillResult, error) {
	q := url.Values{}
	if visibility != "" {
		q.Set("visibility", visibility)
	}
	endpoint := fmt.Sprintf("/api/skills/%s/versions/%s",
		url.PathEscape(slug), url.PathEscape(version))
	if encoded := q.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+endpoint, bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/gzip")
	if upstream != nil {
		// Sentinel: empty Type + empty URL → clear request. Anything else
		// (even a partially-filled struct with just URL) → record/update.
		if upstream.Type == "" && upstream.URL == "" {
			req.Header.Set("X-Clear-Upstream", "true")
		} else {
			payload, err := json.Marshal(upstream)
			if err != nil {
				return nil, fmt.Errorf("marshal upstream metadata: %w", err)
			}
			req.Header.Set("X-Upstream", string(payload))
		}
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, handleErrorResponse(resp, body, fmt.Sprintf("skill %q@%s", slug, version))
	}
	var out UploadSkillResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse upload response: %w", err)
	}
	return &out, nil
}

// AssignSkill grants a user access to a restricted skill.
// POST /api/skills/{slug}/assignments with body {username, version?}.
// Idempotent: re-assigning replaces any prior version pin.
func (c *Client) AssignSkill(slug, username, version string) error {
	body, err := json.Marshal(map[string]string{"username": username, "version": version})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	endpoint := fmt.Sprintf("/api/skills/%s/assignments", url.PathEscape(slug))
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return handleErrorResponse(resp, respBody, fmt.Sprintf("skill %q assign %q", slug, username))
	}
	return nil
}

// UnassignSkill revokes a user's grant.
// DELETE /api/skills/{slug}/assignments/{username}.
func (c *Client) UnassignSkill(slug, username string) error {
	endpoint := fmt.Sprintf("/api/skills/%s/assignments/%s",
		url.PathEscape(slug), url.PathEscape(username))
	req, err := http.NewRequest(http.MethodDelete, c.BaseURL+endpoint, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return handleErrorResponse(resp, body, fmt.Sprintf("skill %q unassign %q", slug, username))
	}
	return nil
}

// YankSkill calls DELETE /api/skills/{slug}. Yank is the default; pass hard=true
// to truly delete (admin only on the relay either way).
func (c *Client) YankSkill(slug string, hard bool) error {
	endpoint := "/api/skills/" + url.PathEscape(slug)
	if hard {
		endpoint += "?hard=true"
	}
	req, err := http.NewRequest(http.MethodDelete, c.BaseURL+endpoint, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return handleErrorResponse(resp, body, fmt.Sprintf("skill %q", slug))
	}
	return nil
}

// PatchSkillInput is the body posted to PATCH /api/skills/{slug} by the
// `arc-sync skill edit` subcommand. Each field is a pointer so the caller can
// distinguish "omit" (skip) from "explicitly set to empty string". The relay
// rejects empty Visibility / DisplayName but accepts an empty Description as
// "clear the description".
type PatchSkillInput struct {
	Visibility  *string `json:"visibility,omitempty"`
	DisplayName *string `json:"display_name,omitempty"`
	Description *string `json:"description,omitempty"`
}

// PatchSkill calls PATCH /api/skills/{slug}. Used by `arc-sync skill edit`
// to flip visibility (public ↔ restricted) or rewrite display_name /
// description without re-uploading a version. Returns the updated skill.
func (c *Client) PatchSkill(slug string, in *PatchSkillInput) (*Skill, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	endpoint := "/api/skills/" + url.PathEscape(slug)
	req, err := http.NewRequest(http.MethodPatch, c.BaseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, &SkillHTTPError{
			Status: resp.StatusCode,
			err:    handleErrorResponse(resp, respBody, fmt.Sprintf("skill %q patch", slug)),
		}
	}
	var wrap struct {
		Skill *Skill `json:"skill"`
	}
	if err := json.Unmarshal(respBody, &wrap); err != nil {
		return nil, fmt.Errorf("parse patch response: %w", err)
	}
	return wrap.Skill, nil
}

// SetUpstreamInput is the body posted to PUT /api/skills/{slug}/upstream by
// the `arc-sync skill set-upstream` subcommand. Field names mirror the JSON
// the relay accepts (see internal/web/skills_handlers.go's upstreamPUTBody).
// Type defaults to "git" server-side when empty.
type SetUpstreamInput struct {
	Type    string `json:"type,omitempty"`
	GitURL  string `json:"git_url"`
	Subpath string `json:"git_subpath,omitempty"`
	Ref     string `json:"git_ref,omitempty"`
}

// SetUpstream calls PUT /api/skills/{slug}/upstream. Used by `arc-sync skill
// set-upstream` to add or replace the upstream-tracking row on an existing
// skill without re-uploading the archive. Returns the relay's response body
// as a parsed map so the caller can echo whatever the server settled on
// (defaulted type, defaulted ref) back to the user.
func (c *Client) SetUpstream(slug string, in *SetUpstreamInput) (map[string]any, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	endpoint := "/api/skills/" + url.PathEscape(slug) + "/upstream"
	req, err := http.NewRequest(http.MethodPut, c.BaseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, &SkillHTTPError{
			Status: resp.StatusCode,
			err:    handleErrorResponse(resp, respBody, fmt.Sprintf("skill %q set-upstream", slug)),
		}
	}
	var out map[string]any
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("parse set-upstream response: %w", err)
	}
	return out, nil
}

// ClearUpstream calls DELETE /api/skills/{slug}/upstream. Used by `arc-sync
// skill clear-upstream` to disable update tracking for a skill. The relay
// also clears any stale `outdated` flag in the same operation.
func (c *Client) ClearUpstream(slug string) error {
	endpoint := "/api/skills/" + url.PathEscape(slug) + "/upstream"
	req, err := http.NewRequest(http.MethodDelete, c.BaseURL+endpoint, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return &SkillHTTPError{
			Status: resp.StatusCode,
			err:    handleErrorResponse(resp, body, fmt.Sprintf("skill %q clear-upstream", slug)),
		}
	}
	return nil
}

// SkillHTTPError lets ListSkills/GetSkill/etc. distinguish 404-not-found from
// network/auth errors at the call site. handleErrorResponse already returns
// useful errors for non-404s; we surface 404 specifically so GetSkill can
// return (nil, nil).
type SkillHTTPError struct {
	Status int
	err    error
}

func (e *SkillHTTPError) Error() string { return e.err.Error() }
func (e *SkillHTTPError) Unwrap() error { return e.err }

// skillGet is the JSON-API GET wrapper used by the read-side skill methods.
func (c *Client) skillGet(endpoint string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		err := handleErrorResponse(resp, body, "skills")
		return nil, &SkillHTTPError{Status: resp.StatusCode, err: err}
	}
	return body, nil
}
