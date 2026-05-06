package web

import (
	"bytes"
	"crypto/rand"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/config"
	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
)

// dashRig is a minimal Handlers wired with the skill store, session store,
// templates, and CSRF secret — just the deps the upstream-form path touches.
// It is package-local (web, not web_test) so the tests can use setUser /
// setSessionID directly without spinning a session-cookie auth dance.
type dashRig struct {
	h        *Handlers
	store    *store.SkillStore
	users    *store.UserStore
	admin    *store.User
	sid      string
	csrfTok  string
}

func newDashRig(t *testing.T) *dashRig {
	t.Helper()
	db := testutil.OpenTestDB(t)
	st := store.NewSkillStore(db)
	users := store.NewUserStore(db)
	sessions := store.NewSessionStore(db)

	admin, err := users.Create("dash-admin", "pw", "admin")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}

	cfg := &config.Config{}
	cfg.Auth.SessionSecret = "test-csrf-secret"
	csrfSecret := []byte(cfg.Auth.SessionSecret)
	if len(csrfSecret) == 0 {
		csrfSecret = make([]byte, 32)
		_, _ = rand.Read(csrfSecret)
	}

	funcMap := template.FuncMap{
		"deref": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
	}

	h := &Handlers{
		cfg:          cfg,
		users:        users,
		sessionStore: sessions,
		skillStore:   st,
		csrfSecret:   csrfSecret,
		tmpls:        make(map[string]*template.Template),
	}
	// Load just the templates the upstream-form error path renders.
	h.tmpls["skill_detail.html"] = template.Must(
		template.New("").Funcs(funcMap).
			ParseFS(templateFS, "templates/layout.html", "templates/skill_detail.html"),
	)

	return &dashRig{h: h, store: st, users: users, admin: admin}
}

// adminReq builds a request with the admin session and a valid CSRF token
// already attached, so the test only has to assert behavior. POST forms get
// the token in csrf_token; the body is the supplied form values.
func (d *dashRig) adminReq(t *testing.T, method, path string, form url.Values) *http.Request {
	t.Helper()
	if d.sid == "" {
		d.sid = "test-session-" + d.admin.ID
	}
	if d.csrfTok == "" {
		d.csrfTok = d.h.csrfToken(d.sid)
	}
	if form != nil {
		form.Set("csrf_token", d.csrfTok)
	}
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if method == http.MethodPost && form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	ctx := setUser(req.Context(), d.admin)
	ctx = setSessionID(ctx, d.sid)
	return req.WithContext(ctx)
}

func (d *dashRig) seedSkill(t *testing.T, slug string) *store.Skill {
	t.Helper()
	sk := &store.Skill{
		Slug:        slug,
		DisplayName: slug,
		Description: "fixture",
		Visibility:  "public",
	}
	if err := d.store.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	return sk
}

// TestDashboardUpstreamForm_Create: POST /skills/{slug}/upstream with a fresh
// skill creates the upstream row and redirects back to the detail page.
func TestDashboardUpstreamForm_Create(t *testing.T) {
	d := newDashRig(t)
	sk := d.seedSkill(t, "form-create")

	form := url.Values{
		"git_url":     {"https://github.com/example/repo"},
		"git_subpath": {"skills/foo"},
		"git_ref":     {"main"},
	}
	req := d.adminReq(t, "POST", "/skills/form-create/upstream", form)
	rw := httptest.NewRecorder()
	d.h.HandleSkillRoutes(rw, req)

	if rw.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rw.Code, rw.Body.String())
	}
	if got := rw.Header().Get("Location"); got != "/skills/form-create" {
		t.Errorf("Location = %q, want /skills/form-create", got)
	}

	u, err := d.store.GetUpstream(sk.ID)
	if err != nil || u == nil {
		t.Fatalf("GetUpstream: u=%v err=%v", u, err)
	}
	if u.GitURL != "https://github.com/example/repo" {
		t.Errorf("GitURL = %q", u.GitURL)
	}
	if u.GitSubpath != "skills/foo" {
		t.Errorf("GitSubpath = %q", u.GitSubpath)
	}
	if u.GitRef != "main" {
		t.Errorf("GitRef = %q", u.GitRef)
	}
}

// TestDashboardUpstreamForm_Replace: posting to a slug that already has an
// upstream row replaces the metadata fields in place.
func TestDashboardUpstreamForm_Replace(t *testing.T) {
	d := newDashRig(t)
	sk := d.seedSkill(t, "form-replace")
	if err := d.store.UpsertUpstream(&store.SkillUpstream{
		SkillID: sk.ID, GitURL: "https://github.com/old/repo", GitRef: "main",
	}); err != nil {
		t.Fatalf("seed UpsertUpstream: %v", err)
	}

	form := url.Values{
		"git_url": {"https://github.com/new/repo"},
		"git_ref": {"feature/x"},
	}
	req := d.adminReq(t, "POST", "/skills/form-replace/upstream", form)
	rw := httptest.NewRecorder()
	d.h.HandleSkillRoutes(rw, req)

	if rw.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rw.Code, rw.Body.String())
	}

	u, _ := d.store.GetUpstream(sk.ID)
	if u == nil || u.GitURL != "https://github.com/new/repo" {
		t.Errorf("GitURL after replace = %v", u)
	}
	if u != nil && u.GitRef != "feature/x" {
		t.Errorf("GitRef after replace = %q", u.GitRef)
	}
}

// TestDashboardUpstreamForm_MissingURL: empty git_url re-renders the detail
// page with the error message and HTTP 200 (not a redirect, so the user can
// see what went wrong without losing their other field values).
func TestDashboardUpstreamForm_MissingURL(t *testing.T) {
	d := newDashRig(t)
	d.seedSkill(t, "form-no-url")

	form := url.Values{
		"git_url":     {""},
		"git_subpath": {"skills/foo"},
	}
	req := d.adminReq(t, "POST", "/skills/form-no-url/upstream", form)
	rw := httptest.NewRecorder()
	d.h.HandleSkillRoutes(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-render); body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, "Upstream git URL is required") {
		t.Errorf("body missing error message; got %s", body)
	}
	// The user's typed subpath should be preserved on re-render so they don't
	// have to retype it.
	if !strings.Contains(body, `value="skills/foo"`) {
		t.Errorf("body should preserve user's subpath value; got %s", body)
	}
}

// TestDashboardUpstreamForm_Clear: POST /skills/{slug}/upstream/clear deletes
// the row and redirects back to the detail page.
func TestDashboardUpstreamForm_Clear(t *testing.T) {
	d := newDashRig(t)
	sk := d.seedSkill(t, "form-clear")
	if err := d.store.UpsertUpstream(&store.SkillUpstream{
		SkillID: sk.ID, GitURL: "https://github.com/example/repo", GitRef: "HEAD",
	}); err != nil {
		t.Fatalf("seed UpsertUpstream: %v", err)
	}

	req := d.adminReq(t, "POST", "/skills/form-clear/upstream/clear", url.Values{})
	rw := httptest.NewRecorder()
	d.h.HandleSkillRoutes(rw, req)

	if rw.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rw.Code, rw.Body.String())
	}
	if got := rw.Header().Get("Location"); got != "/skills/form-clear" {
		t.Errorf("Location = %q", got)
	}

	u, _ := d.store.GetUpstream(sk.ID)
	if u != nil {
		t.Errorf("upstream row should be deleted, got %+v", u)
	}
}

// TestDashboardUpstreamForm_NonAdminForbidden: a non-admin user POSTing the
// form gets 403 — the existing requireAdmin gate in HandleSkillRoutes covers
// every mutation including these new paths.
func TestDashboardUpstreamForm_NonAdminForbidden(t *testing.T) {
	d := newDashRig(t)
	d.seedSkill(t, "form-nonadmin")
	regular, err := d.users.Create("alice", "pw", "user")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	sid := "alice-session"
	form := url.Values{
		"csrf_token": {d.h.csrfToken(sid)},
		"git_url":    {"https://github.com/example/repo"},
	}
	req := httptest.NewRequest("POST", "/skills/form-nonadmin/upstream",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := setUser(req.Context(), regular)
	ctx = setSessionID(ctx, sid)
	req = req.WithContext(ctx)

	rw := httptest.NewRecorder()
	d.h.HandleSkillRoutes(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rw.Code, rw.Body.String())
	}
}

// TestDashboardUpstreamForm_BadCSRF: missing/invalid CSRF token → 403. The
// existing HandleSkillRoutes mutation gate catches this; we assert it covers
// the upstream paths too.
func TestDashboardUpstreamForm_BadCSRF(t *testing.T) {
	d := newDashRig(t)
	d.seedSkill(t, "form-csrf")

	body := strings.NewReader(`csrf_token=wrong&git_url=https://github.com/example/repo`)
	req := httptest.NewRequest("POST", "/skills/form-csrf/upstream", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := setUser(req.Context(), d.admin)
	ctx = setSessionID(ctx, "csrf-test-session")
	req = req.WithContext(ctx)

	rw := httptest.NewRecorder()
	d.h.HandleSkillRoutes(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rw.Code, rw.Body.String())
	}
}

// TestDashboardMetaForm_VisibilityFlip: POST /skills/{slug}/meta flips
// visibility from public to restricted and redirects back to the detail page.
func TestDashboardMetaForm_VisibilityFlip(t *testing.T) {
	d := newDashRig(t)
	sk := d.seedSkill(t, "meta-flip")
	if sk.Visibility != "public" {
		t.Fatalf("seed visibility = %q, want public", sk.Visibility)
	}

	form := url.Values{
		"display_name": {sk.DisplayName},
		"description":  {sk.Description},
		"visibility":   {"restricted"},
	}
	req := d.adminReq(t, "POST", "/skills/meta-flip/meta", form)
	rw := httptest.NewRecorder()
	d.h.HandleSkillRoutes(rw, req)

	if rw.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rw.Code, rw.Body.String())
	}
	if got := rw.Header().Get("Location"); got != "/skills/meta-flip" {
		t.Errorf("Location = %q", got)
	}
	post, _ := d.store.GetSkillBySlug("meta-flip")
	if post == nil || post.Visibility != "restricted" {
		t.Errorf("Visibility after POST = %v, want restricted", post)
	}
}

// TestDashboardMetaForm_RenameAndDescribe: POST patches both display_name
// and description in one shot.
func TestDashboardMetaForm_RenameAndDescribe(t *testing.T) {
	d := newDashRig(t)
	sk := d.seedSkill(t, "meta-rename")

	form := url.Values{
		"display_name": {"Renamed Skill"},
		"description":  {"Now with extra context"},
		"visibility":   {sk.Visibility},
	}
	req := d.adminReq(t, "POST", "/skills/meta-rename/meta", form)
	rw := httptest.NewRecorder()
	d.h.HandleSkillRoutes(rw, req)

	if rw.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rw.Code, rw.Body.String())
	}
	post, _ := d.store.GetSkillBySlug("meta-rename")
	if post == nil {
		t.Fatal("skill missing after POST")
	}
	if post.DisplayName != "Renamed Skill" {
		t.Errorf("DisplayName = %q", post.DisplayName)
	}
	if post.Description != "Now with extra context" {
		t.Errorf("Description = %q", post.Description)
	}
}

// TestDashboardMetaForm_EmptyDisplayNameRejected: empty display_name
// re-renders the detail page with the error rather than redirecting.
func TestDashboardMetaForm_EmptyDisplayNameRejected(t *testing.T) {
	d := newDashRig(t)
	d.seedSkill(t, "meta-empty-name")

	form := url.Values{
		"display_name": {"   "},
		"description":  {"x"},
		"visibility":   {"public"},
	}
	req := d.adminReq(t, "POST", "/skills/meta-empty-name/meta", form)
	rw := httptest.NewRecorder()
	d.h.HandleSkillRoutes(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-render); body=%s", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "Display name is required") {
		t.Errorf("body missing error message; body=%s", rw.Body.String())
	}
}

// TestDashboardMetaForm_BadVisibility: invalid visibility re-renders the
// detail page with the error rather than redirecting.
func TestDashboardMetaForm_BadVisibility(t *testing.T) {
	d := newDashRig(t)
	d.seedSkill(t, "meta-bad-vis")

	form := url.Values{
		"display_name": {"X"},
		"description":  {""},
		"visibility":   {"halfway"},
	}
	req := d.adminReq(t, "POST", "/skills/meta-bad-vis/meta", form)
	rw := httptest.NewRecorder()
	d.h.HandleSkillRoutes(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-render); body=%s", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "Visibility must be public or restricted") {
		t.Errorf("body missing error message; body=%s", rw.Body.String())
	}
}

// TestDashboardMetaForm_NonAdminForbidden: regular users get 403 from the
// existing requireAdmin gate before the meta handler runs.
func TestDashboardMetaForm_NonAdminForbidden(t *testing.T) {
	d := newDashRig(t)
	d.seedSkill(t, "meta-gate")
	regular, err := d.users.Create("alice-meta", "pw", "user")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	sid := "alice-meta-session"
	form := url.Values{
		"csrf_token":   {d.h.csrfToken(sid)},
		"display_name": {"Hijacked"},
		"description":  {""},
		"visibility":   {"public"},
	}
	req := httptest.NewRequest("POST", "/skills/meta-gate/meta",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := setUser(req.Context(), regular)
	ctx = setSessionID(ctx, sid)
	req = req.WithContext(ctx)

	rw := httptest.NewRecorder()
	d.h.HandleSkillRoutes(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rw.Code, rw.Body.String())
	}
}

// TestDashboardSkillDetail_RendersEditMetaCard: a GET as admin shows the
// "Edit metadata" admin card with the form pointed at the meta endpoint.
func TestDashboardSkillDetail_RendersEditMetaCard(t *testing.T) {
	d := newDashRig(t)
	sk := d.seedSkill(t, "meta-render")

	req := d.adminReq(t, "GET", "/skills/meta-render", nil)
	rw := httptest.NewRecorder()
	d.h.HandleSkillRoutes(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, "Edit metadata") {
		t.Errorf("detail page missing Edit metadata card")
	}
	if !strings.Contains(body, `action="/skills/meta-render/meta"`) {
		t.Errorf("detail page missing meta form action")
	}
	if !strings.Contains(body, `name="visibility"`) {
		t.Errorf("detail page missing visibility select")
	}
	// The current visibility should be pre-selected.
	wantSelected := `value="` + sk.Visibility + `"`
	if !strings.Contains(body, wantSelected) {
		t.Errorf("detail page should pre-select current visibility %q", sk.Visibility)
	}
}

// TestDashboardSkillDetail_RendersAdminForm_NoUpstream: a GET on the detail
// page when no upstream is configured renders the admin "enable tracking"
// form. Smoke check that the template branch we changed actually emits.
func TestDashboardSkillDetail_RendersAdminForm_NoUpstream(t *testing.T) {
	d := newDashRig(t)
	d.seedSkill(t, "detail-no-upstream")

	req := d.adminReq(t, "GET", "/skills/detail-no-upstream", nil)
	rw := httptest.NewRecorder()
	d.h.HandleSkillRoutes(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, `action="/skills/detail-no-upstream/upstream"`) {
		t.Errorf("detail page missing upstream form action; body=%s", body)
	}
	if !strings.Contains(body, "Enable upstream tracking") {
		t.Errorf("detail page missing 'Enable upstream tracking' button")
	}
	if !strings.Contains(body, `name="git_url"`) {
		t.Errorf("detail page missing git_url input")
	}
}

// TestDashboardSkillDetail_RendersEditForm_WithUpstream: when an upstream is
// configured, the detail page exposes the inline edit form (collapsed inside
// <details>) and the "Clear tracking" button.
func TestDashboardSkillDetail_RendersEditForm_WithUpstream(t *testing.T) {
	d := newDashRig(t)
	sk := d.seedSkill(t, "detail-with-upstream")
	if err := d.store.UpsertUpstream(&store.SkillUpstream{
		SkillID: sk.ID, GitURL: "https://github.com/example/repo",
		GitSubpath: "skills/foo", GitRef: "main",
	}); err != nil {
		t.Fatalf("UpsertUpstream: %v", err)
	}

	req := d.adminReq(t, "GET", "/skills/detail-with-upstream", nil)
	rw := httptest.NewRecorder()
	d.h.HandleSkillRoutes(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, "Edit upstream tracking") {
		t.Errorf("detail page missing edit toggle; body=%s", body)
	}
	if !strings.Contains(body, `formaction="/skills/detail-with-upstream/upstream/clear"`) {
		t.Errorf("detail page missing clear-tracking action")
	}
	// Existing values prefilled in the edit form so admin can tweak them.
	if !bytes.Contains(rw.Body.Bytes(), []byte(`value="https://github.com/example/repo"`)) {
		t.Errorf("edit form should prefill GitURL")
	}
	if !bytes.Contains(rw.Body.Bytes(), []byte(`value="skills/foo"`)) {
		t.Errorf("edit form should prefill subpath")
	}
}
