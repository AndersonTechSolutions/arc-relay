package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// setupCheckDriftMock spins up an httptest server that mimics the relay's
// POST /api/skills/{slug}/check-drift endpoint. The behavior keys off the
// slug:
//   - "drift-skill"       → 200 + drift body
//   - "uptodate-skill"    → 204
//   - "no-upstream-skill" → 409
//   - "missing-skill"     → 404
//   - "broken-skill"      → 502
//   - anything else       → 500 (catches unexpected requests)
func setupCheckDriftMock(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		// Path shape: /api/skills/{slug}/check-drift
		const prefix = "/api/skills/"
		const suffix = "/check-drift"
		if len(r.URL.Path) <= len(prefix)+len(suffix) ||
			r.URL.Path[:len(prefix)] != prefix ||
			r.URL.Path[len(r.URL.Path)-len(suffix):] != suffix {
			http.NotFound(w, r)
			return
		}
		slug := r.URL.Path[len(prefix) : len(r.URL.Path)-len(suffix)]

		switch slug {
		case "drift-skill":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"outdated": true,
				"drift": map[string]any{
					"severity":           "minor",
					"summary":            "3 commits behind",
					"recommended_action": "run `arc-sync skill push`",
					"commits_ahead":      3,
					"upstream_sha":       "abc123",
					"detected_at":        "2026-04-30T12:00:00Z",
					"relay_version":      "0.1.0",
					"relay_hash":         "ef12",
					"llm_model":          "gpt-4o-mini",
				},
			})
		case "uptodate-skill":
			w.WriteHeader(http.StatusNoContent)
		case "no-upstream-skill":
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no upstream configured"})
		case "missing-skill":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "skill not found"})
		case "broken-skill":
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "upstream fetch failed"})
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}

func TestCheckDrift_Drift(t *testing.T) {
	ts := setupCheckDriftMock(t)
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	drift, err := c.CheckDrift("drift-skill")
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if drift == nil {
		t.Fatal("expected drift block, got nil")
	}
	if drift.Severity != "minor" {
		t.Errorf("Severity = %q, want %q", drift.Severity, "minor")
	}
	if drift.Summary != "3 commits behind" {
		t.Errorf("Summary = %q", drift.Summary)
	}
	if drift.RecommendedAction != "run `arc-sync skill push`" {
		t.Errorf("RecommendedAction = %q", drift.RecommendedAction)
	}
	if drift.CommitsAhead != 3 {
		t.Errorf("CommitsAhead = %d, want 3", drift.CommitsAhead)
	}
	if drift.UpstreamSHA != "abc123" {
		t.Errorf("UpstreamSHA = %q", drift.UpstreamSHA)
	}
	want, _ := time.Parse(time.RFC3339, "2026-04-30T12:00:00Z")
	if !drift.DetectedAt.Equal(want) {
		t.Errorf("DetectedAt = %v, want %v", drift.DetectedAt, want)
	}
	if drift.LLMModel != "gpt-4o-mini" {
		t.Errorf("LLMModel = %q", drift.LLMModel)
	}
}

func TestCheckDrift_UpToDate(t *testing.T) {
	ts := setupCheckDriftMock(t)
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	drift, err := c.CheckDrift("uptodate-skill")
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if drift != nil {
		t.Errorf("expected nil drift on 204, got %+v", drift)
	}
}

func TestCheckDrift_NoUpstream(t *testing.T) {
	ts := setupCheckDriftMock(t)
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	_, err := c.CheckDrift("no-upstream-skill")
	if err == nil {
		t.Fatal("expected error on 409")
	}
	var httpErr *SkillHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *SkillHTTPError, got %T: %v", err, err)
	}
	if httpErr.Status != http.StatusConflict {
		t.Errorf("Status = %d, want %d", httpErr.Status, http.StatusConflict)
	}
}

func TestCheckDrift_NotFound(t *testing.T) {
	ts := setupCheckDriftMock(t)
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	_, err := c.CheckDrift("missing-skill")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	var httpErr *SkillHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *SkillHTTPError, got %T: %v", err, err)
	}
	if httpErr.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want %d", httpErr.Status, http.StatusNotFound)
	}
}

func TestCheckDrift_UpstreamFetchFailed(t *testing.T) {
	ts := setupCheckDriftMock(t)
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	_, err := c.CheckDrift("broken-skill")
	if err == nil {
		t.Fatal("expected error on 502")
	}
	var httpErr *SkillHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *SkillHTTPError, got %T: %v", err, err)
	}
	if httpErr.Status != http.StatusBadGateway {
		t.Errorf("Status = %d, want %d", httpErr.Status, http.StatusBadGateway)
	}
}

// TestSetUpstream_RoundTrip verifies the PUT shape: the client serialises
// SetUpstreamInput as expected by the relay handler, sets the right Content-
// Type, and parses the body back into a map.
func TestSetUpstream_RoundTrip(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotCT     string
		gotBody   map[string]any
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"skill_id":      "id-123",
			"upstream_type": "git",
			"git_url":       "https://github.com/example/repo",
			"git_subpath":   "skills/foo",
			"git_ref":       "main",
		})
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	resp, err := c.SetUpstream("foo", &SetUpstreamInput{
		GitURL:  "https://github.com/example/repo",
		Subpath: "skills/foo",
		Ref:     "main",
	})
	if err != nil {
		t.Fatalf("SetUpstream: %v", err)
	}
	if gotMethod != "PUT" {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/api/skills/foo/upstream" {
		t.Errorf("path = %q", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody["git_url"] != "https://github.com/example/repo" {
		t.Errorf("body git_url = %v", gotBody["git_url"])
	}
	if gotBody["git_subpath"] != "skills/foo" {
		t.Errorf("body git_subpath = %v", gotBody["git_subpath"])
	}
	if gotBody["git_ref"] != "main" {
		t.Errorf("body git_ref = %v", gotBody["git_ref"])
	}
	if resp["git_url"] != "https://github.com/example/repo" {
		t.Errorf("resp git_url = %v", resp["git_url"])
	}
}

// TestSetUpstream_404 wraps the relay's 404 in a SkillHTTPError so the CLI
// can pretty-print it.
func TestSetUpstream_404(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "skill not found"})
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	_, err := c.SetUpstream("ghost", &SetUpstreamInput{GitURL: "https://github.com/example/repo"})
	if err == nil {
		t.Fatal("expected error on 404")
	}
	var httpErr *SkillHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *SkillHTTPError, got %T: %v", err, err)
	}
	if httpErr.Status != http.StatusNotFound {
		t.Errorf("Status = %d", httpErr.Status)
	}
}

// TestClearUpstream_RoundTrip verifies the DELETE call.
func TestClearUpstream_RoundTrip(t *testing.T) {
	var gotMethod, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]bool{"cleared": true})
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	if err := c.ClearUpstream("foo"); err != nil {
		t.Fatalf("ClearUpstream: %v", err)
	}
	if gotMethod != "DELETE" {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/skills/foo/upstream" {
		t.Errorf("path = %q", gotPath)
	}
}

// TestClearUpstream_404 surfaces a SkillHTTPError on a missing slug.
func TestClearUpstream_404(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	err := c.ClearUpstream("ghost")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	var httpErr *SkillHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *SkillHTTPError, got %T: %v", err, err)
	}
	if httpErr.Status != http.StatusNotFound {
		t.Errorf("Status = %d", httpErr.Status)
	}
}

// TestPatchSkill_RoundTrip verifies the PATCH shape end-to-end: PATCH method,
// /api/skills/{slug} path, application/json content type, body shape, and
// the wrapped {"skill":...} response.
func TestPatchSkill_RoundTrip(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotCT     string
		gotBody   map[string]any
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"skill": map[string]any{
				"id":           "id-123",
				"slug":         "foo",
				"display_name": "Renamed",
				"description":  "",
				"visibility":   "public",
				"created_at":   "2026-04-30T00:00:00Z",
				"updated_at":   "2026-05-06T17:00:00Z",
			},
		})
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	v := "public"
	dn := "Renamed"
	updated, err := c.PatchSkill("foo", &PatchSkillInput{Visibility: &v, DisplayName: &dn})
	if err != nil {
		t.Fatalf("PatchSkill: %v", err)
	}
	if gotMethod != "PATCH" {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/api/skills/foo" {
		t.Errorf("path = %q", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody["visibility"] != "public" {
		t.Errorf("body visibility = %v", gotBody["visibility"])
	}
	if gotBody["display_name"] != "Renamed" {
		t.Errorf("body display_name = %v", gotBody["display_name"])
	}
	if updated == nil || updated.Visibility != "public" || updated.DisplayName != "Renamed" {
		t.Errorf("updated = %+v", updated)
	}
}

// TestPatchSkill_OmitsAbsentFields verifies that nil-pointer fields don't
// leak into the JSON body — important so partial updates don't silently
// blank server-side values via "" coercion.
func TestPatchSkill_OmitsAbsentFields(t *testing.T) {
	var rawBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"skill": map[string]any{
				"id": "id-123", "slug": "foo", "visibility": "public",
				"display_name": "x", "description": "",
				"created_at": "2026-04-30T00:00:00Z",
				"updated_at": "2026-05-06T17:00:00Z",
			},
		})
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	v := "public"
	if _, err := c.PatchSkill("foo", &PatchSkillInput{Visibility: &v}); err != nil {
		t.Fatalf("PatchSkill: %v", err)
	}
	if !bytes.Contains(rawBody, []byte(`"visibility":"public"`)) {
		t.Errorf("body should contain visibility; got %s", rawBody)
	}
	if bytes.Contains(rawBody, []byte("display_name")) {
		t.Errorf("body should NOT contain display_name when nil; got %s", rawBody)
	}
	if bytes.Contains(rawBody, []byte("description")) {
		t.Errorf("body should NOT contain description when nil; got %s", rawBody)
	}
}

// TestPatchSkill_400 surfaces a SkillHTTPError on validation failure.
func TestPatchSkill_400(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "visibility must be public/restricted"})
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	v := "halfway"
	_, err := c.PatchSkill("foo", &PatchSkillInput{Visibility: &v})
	if err == nil {
		t.Fatal("expected error on 400")
	}
	var httpErr *SkillHTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadRequest {
		t.Errorf("expected SkillHTTPError 400, got %T %v", err, err)
	}
}

// TestSkill_OutdatedJSON verifies the JSON shape: when the relay sends
// `outdated:1` + `drift:{...}` on a list/detail row, our Skill struct picks
// both up. This guards against silent wire-shape regressions.
func TestSkill_OutdatedJSON(t *testing.T) {
	raw := []byte(`{
		"id":"id1","slug":"foo","display_name":"Foo","description":"",
		"visibility":"public","latest_version":"1.0.0",
		"created_at":"2026-04-30T00:00:00Z","updated_at":"2026-04-30T00:00:00Z",
		"outdated":1,
		"drift":{"severity":"security","summary":"CVE-2024-12345","recommended_action":"upgrade now","commits_ahead":7,"upstream_sha":"deadbeef","detected_at":"2026-04-30T12:00:00Z","relay_version":"0.2.0","relay_hash":"abcd","llm_model":"gpt-4o-mini"}
	}`)
	var s Skill
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Outdated != 1 {
		t.Errorf("Outdated = %d, want 1", s.Outdated)
	}
	if s.Drift == nil {
		t.Fatal("expected non-nil Drift block")
	}
	if s.Drift.Severity != "security" {
		t.Errorf("Drift.Severity = %q, want %q", s.Drift.Severity, "security")
	}
	if s.Drift.CommitsAhead != 7 {
		t.Errorf("Drift.CommitsAhead = %d, want 7", s.Drift.CommitsAhead)
	}
}
