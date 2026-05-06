package web_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/comma-compliance/arc-relay/internal/config"
	"github.com/comma-compliance/arc-relay/internal/server"
	"github.com/comma-compliance/arc-relay/internal/skills"
	"github.com/comma-compliance/arc-relay/internal/skills/checker"
	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
	"github.com/comma-compliance/arc-relay/internal/web"
)

const skillMD = `---
name: demo-skill
description: Demo skill for handler tests.
user-invocable: true
---

# Demo
`

// skillsRig wires up SkillsHandlers + a test mux that lets each subtest pick
// the user injected into context. Returns the mux and the underlying skill
// store so tests can seed data without hitting the upload path.
type skillsRig struct {
	mux            *http.ServeMux
	store          *store.SkillStore
	svc            *skills.Service
	users          *store.UserStore
	admin          *store.User
	userToInject   *store.User
	apiKeyToInject *store.APIKey
	checker        *checker.Service
	cacheDir       string
}

func newSkillsRig(t *testing.T) *skillsRig {
	t.Helper()
	return newSkillsRigWithChecker(t, nil, "")
}

// newSkillsRigWithCheckerEnabled wires a fresh test rig with a real
// checker.Service backed by a tempdir cache. Used by the check-drift tests.
// The returned rig's `store` and `cacheDir` fields are exposed so tests can
// seed skills, upstream rows, and prime baselines.
func newSkillsRigWithCheckerEnabled(t *testing.T) *skillsRig {
	t.Helper()
	cacheDir := filepath.Join(t.TempDir(), "upstream-cache")
	// We need a SkillStore + skills.Service to construct checker.Service, but
	// the rig's helpers create those. To keep the wiring linear we build a
	// fresh DB here and pass everything through. testutil.OpenTestFileDB is
	// required because the checker may open multiple connections.
	db := testutil.OpenTestFileDB(t)
	st := store.NewSkillStore(db)
	bundlesDir := t.TempDir()
	svc := skills.New(st, bundlesDir)
	users := store.NewUserStore(db)

	admin, err := users.Create("test-admin", "test-pw", "admin")
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	chk := checker.NewService(st, svc, nil, config.SkillsCheckerConfig{
		Enabled:          true,
		UpstreamCacheDir: cacheDir,
		LLMDiffMaxBytes:  4096,
		// Tight clone timeout so the 502 test fails fast instead of hanging
		// on a hostname lookup the way the cron's 60s default would.
		GitCloneTimeout: 5 * time.Second,
	})

	h := web.NewSkillsHandlers(svc, st, users, chk,
		func(ctx context.Context) *store.User { return server.UserFromContext(ctx) },
		func(ctx context.Context) *store.APIKey { return server.APIKeyFromContext(ctx) },
	)

	rig := &skillsRig{
		store: st, svc: svc, users: users, admin: admin,
		checker: chk, cacheDir: cacheDir, mux: http.NewServeMux(),
	}
	wrap := func(handler http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if rig.userToInject != nil {
				ctx = server.WithUser(ctx, rig.userToInject)
			}
			if rig.apiKeyToInject != nil {
				ctx = server.WithAPIKey(ctx, rig.apiKeyToInject)
			}
			handler(w, r.WithContext(ctx))
		})
	}
	rig.mux.Handle("/api/skills", wrap(h.HandleSkills))
	rig.mux.Handle("/api/skills/assigned", wrap(h.HandleAssigned))
	rig.mux.Handle("/api/skills/", wrap(h.HandleSkillByPath))
	return rig
}

// newSkillsRigWithChecker wires a rig with an optional checker. cacheDir is
// retained on the rig for tests that need to prime caches. Currently used
// only by the public newSkillsRig (which passes a nil checker).
func newSkillsRigWithChecker(t *testing.T, chk *checker.Service, cacheDir string) *skillsRig {
	t.Helper()
	db := testutil.OpenTestDB(t)
	st := store.NewSkillStore(db)
	svc := skills.New(st, t.TempDir())
	users := store.NewUserStore(db)

	// Seed an admin user so audit FKs (skills.created_by, skill_versions.uploaded_by)
	// can resolve. Without this, every upload trips ON DELETE SET NULL → FK violation.
	admin, err := users.Create("test-admin", "test-pw", "admin")
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	h := web.NewSkillsHandlers(svc, st, users, chk,
		func(ctx context.Context) *store.User { return server.UserFromContext(ctx) },
		func(ctx context.Context) *store.APIKey { return server.APIKeyFromContext(ctx) },
	)

	rig := &skillsRig{store: st, svc: svc, users: users, admin: admin, checker: chk, cacheDir: cacheDir, mux: http.NewServeMux()}
	wrap := func(handler http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if rig.userToInject != nil {
				ctx = server.WithUser(ctx, rig.userToInject)
			}
			if rig.apiKeyToInject != nil {
				ctx = server.WithAPIKey(ctx, rig.apiKeyToInject)
			}
			handler(w, r.WithContext(ctx))
		})
	}
	rig.mux.Handle("/api/skills", wrap(h.HandleSkills))
	rig.mux.Handle("/api/skills/assigned", wrap(h.HandleAssigned))
	rig.mux.Handle("/api/skills/", wrap(h.HandleSkillByPath))
	return rig
}

// regularUser builds a fake non-admin user, with an existing DB row so FKs
// resolve when the user uploads.
func (r *skillsRig) regularUser(t *testing.T, username string) *store.User {
	t.Helper()
	u, err := r.users.Create(username, "test-pw", "user")
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return u
}

// makeArchive builds a minimal gzipped tar with one SKILL.md at root.
func makeArchive(t *testing.T, frontmatter string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "SKILL.md", Mode: 0o644, Size: int64(len(frontmatter)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write([]byte(frontmatter)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestSkillsHandlers_RequiresAuth(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = nil

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills", nil))
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/skills unauth = %d, want 401", rw.Code)
	}
}

func TestSkillsHandlers_UploadAdminOnly(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.regularUser(t, "ian")

	archive := makeArchive(t, skillMD)
	req := httptest.NewRequest("POST", "/api/skills/demo-skill/versions/1.0.0",
		bytes.NewReader(archive))
	req.Header.Set("Content-Type", "application/gzip")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusForbidden {
		t.Errorf("non-admin upload = %d, want 403; body=%s", rw.Code, rw.Body.String())
	}
}

func TestSkillsHandlers_UploadHappyPath(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	archive := makeArchive(t, skillMD)
	req := httptest.NewRequest("POST", "/api/skills/demo-skill/versions/1.0.0?visibility=public",
		bytes.NewReader(archive))
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusCreated {
		t.Fatalf("upload = %d, body=%s", rw.Code, rw.Body.String())
	}
	var res skills.UploadResult
	if err := json.Unmarshal(rw.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.Skill.Slug != "demo-skill" {
		t.Errorf("slug = %q", res.Skill.Slug)
	}
	if res.Skill.Visibility != "public" {
		t.Errorf("visibility = %q", res.Skill.Visibility)
	}
	if res.Version.Version != "1.0.0" {
		t.Errorf("version = %q", res.Version.Version)
	}
}

func TestSkillsHandlers_UploadOversize(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	big := bytes.Repeat([]byte{0}, skills.MaxArchiveSize+10)
	req := httptest.NewRequest("POST", "/api/skills/big-skill/versions/1.0.0",
		bytes.NewReader(big))
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize upload = %d, want 413; body=%s", rw.Code, rw.Body.String())
	}
}

func TestSkillsHandlers_UploadDuplicateVersion(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	archive := makeArchive(t, skillMD)
	req := httptest.NewRequest("POST", "/api/skills/demo-skill/versions/1.0.0",
		bytes.NewReader(archive))
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusCreated {
		t.Fatalf("first upload = %d", rw.Code)
	}

	req = httptest.NewRequest("POST", "/api/skills/demo-skill/versions/1.0.0",
		bytes.NewReader(archive))
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusConflict {
		t.Errorf("duplicate upload = %d, want 409", rw.Code)
	}
}

func TestSkillsHandlers_GetSkill(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	// Seed via service.
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "public",
	}); err != nil {
		t.Fatalf("seed upload: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/demo-skill", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("GET = %d", rw.Code)
	}
	var resp struct {
		Skill    *store.Skill          `json:"skill"`
		Versions []*store.SkillVersion `json:"versions"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Skill.Slug != "demo-skill" {
		t.Errorf("slug = %q", resp.Skill.Slug)
	}
	if len(resp.Versions) != 1 {
		t.Errorf("versions len = %d", len(resp.Versions))
	}
}

func TestSkillsHandlers_DownloadArchive_RoundTrip(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	original := makeArchive(t, skillMD)
	res, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: original, Visibility: "public",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET",
		"/api/skills/demo-skill/versions/1.0.0/archive", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("download = %d body=%s", rw.Code, rw.Body.String())
	}
	if !bytes.Equal(rw.Body.Bytes(), original) {
		t.Errorf("downloaded bytes differ from upload")
	}
	if got := rw.Header().Get("X-Skill-SHA256"); got != res.Version.ArchiveSHA256 {
		t.Errorf("X-Skill-SHA256 = %q, want %q", got, res.Version.ArchiveSHA256)
	}
	if got := rw.Header().Get("Content-Type"); got != "application/gzip" {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestSkillsHandlers_AssignedFiltersByVisibility(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "public",
	}); err != nil {
		t.Fatalf("upload public: %v", err)
	}
	secretMD := strings.Replace(skillMD, "demo-skill", "secret-skill", 1)
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, secretMD), Visibility: "restricted",
	}); err != nil {
		t.Fatalf("upload restricted: %v", err)
	}

	// Switch to a regular user; they should see only the public skill.
	rig.userToInject = rig.regularUser(t, "user7")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/assigned", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("assigned = %d body=%s", rw.Code, rw.Body.String())
	}
	var resp struct {
		Assigned []*store.AssignedSkill `json:"assigned"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Assigned) != 1 || resp.Assigned[0].Skill.Slug != "demo-skill" {
		t.Fatalf("expected only demo-skill, got %+v", resp.Assigned)
	}
}

func TestSkillsHandlers_RegularUserCannotSeeRestrictedDirectly(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	secretMD := strings.Replace(skillMD, "demo-skill", "secret-skill", 1)
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, secretMD), Visibility: "restricted",
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	rig.userToInject = rig.regularUser(t, "outsider")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/secret-skill", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("non-admin GET on restricted = %d, want 404", rw.Code)
	}
}

func TestSkillsHandlers_AdminListSeesAll(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "public",
	}); err != nil {
		t.Fatalf("upload public: %v", err)
	}
	secretMD := strings.Replace(skillMD, "demo-skill", "secret-skill", 1)
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, secretMD), Visibility: "restricted",
	}); err != nil {
		t.Fatalf("upload restricted: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("list = %d body=%s", rw.Code, rw.Body.String())
	}
	var resp struct {
		Skills []*store.Skill `json:"skills"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Skills) != 2 {
		t.Errorf("admin list len = %d, want 2", len(resp.Skills))
	}
}

func TestSkillsHandlers_YankSkill(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "public",
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("DELETE", "/api/skills/demo-skill", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("yank = %d body=%s", rw.Code, rw.Body.String())
	}

	got, _ := rig.store.GetSkillBySlug("demo-skill")
	if got == nil || got.YankedAt == nil {
		t.Errorf("skill should be present and yanked, got %+v", got)
	}

	// Hard delete removes it.
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw,
		httptest.NewRequest("DELETE", "/api/skills/demo-skill?hard=true", nil))
	if rw.Code != http.StatusNoContent {
		t.Fatalf("hard delete = %d", rw.Code)
	}
	got, _ = rig.store.GetSkillBySlug("demo-skill")
	if got != nil {
		t.Errorf("skill should be deleted, got %+v", got)
	}
}

func TestSkillsHandlers_YankVersion(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw,
		httptest.NewRequest("DELETE", "/api/skills/demo-skill/versions/1.0.0", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("yank version = %d body=%s", rw.Code, rw.Body.String())
	}
	sk, _ := rig.store.GetSkillBySlug("demo-skill")
	v, _ := rig.store.GetVersion(sk.ID, "1.0.0")
	if v == nil || v.YankedAt == nil {
		t.Errorf("version should be yanked, got %+v", v)
	}
}

func TestSkillsHandlers_BadVersionFormat(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	archive := makeArchive(t, skillMD)
	req := httptest.NewRequest("POST", "/api/skills/demo-skill/versions/latest",
		bytes.NewReader(archive))
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("bad-version upload = %d, want 400; body=%s", rw.Code, rw.Body.String())
	}
}

func TestSkillsHandlers_AssignedRouteNotShadowedByPath(t *testing.T) {
	// Regression: ensure /api/skills/assigned hits HandleAssigned, not
	// HandleSkillByPath, even though both prefixes overlap. The ServeMux
	// resolves longer-pattern wins, but only because we register both.
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/assigned", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("assigned = %d body=%s", rw.Code, rw.Body.String())
	}
	// Body should be {"assigned": [...]}, NOT a "skill not found" response from
	// HandleSkillByPath treating "assigned" as a slug.
	if !bytes.Contains(rw.Body.Bytes(), []byte(`"assigned":`)) {
		t.Errorf("response body did not match HandleAssigned shape: %s", rw.Body.String())
	}
}

// readGzipFirstFile is a small helper used to peek into the downloaded archive
// and confirm the body really is the gzipped tar we uploaded.
func readGzipFirstFile(t *testing.T, b []byte) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar next: %v", err)
	}
	contents, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("tar read: %v", err)
	}
	return hdr.Name + ":" + string(contents)
}

func TestSkillsHandlers_AssignmentLifecycle(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	// Seed a restricted skill so visibility-gated reads matter.
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "restricted",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	alice := rig.regularUser(t, "alice")

	// Pre-grant: alice cannot see the restricted skill.
	rig.userToInject = alice
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/demo-skill", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("pre-grant non-admin GET = %d, want 404", rw.Code)
	}

	// Admin grants alice access (with a version pin).
	rig.userToInject = rig.admin
	body := `{"username":"alice","version":"1.0.0"}`
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/demo-skill/assignments", strings.NewReader(body)))
	if rw.Code != http.StatusCreated {
		t.Fatalf("assign = %d body=%s", rw.Code, rw.Body.String())
	}

	// Post-grant: alice now sees it.
	rig.userToInject = alice
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/demo-skill", nil))
	if rw.Code != http.StatusOK {
		t.Errorf("post-grant non-admin GET = %d, want 200", rw.Code)
	}

	// Admin lists assignments and sees alice.
	rig.userToInject = rig.admin
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/demo-skill/assignments", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("list assignments = %d", rw.Code)
	}
	if !bytes.Contains(rw.Body.Bytes(), []byte(`"user_id":"`+alice.ID+`"`)) {
		t.Errorf("list missing alice: %s", rw.Body.String())
	}

	// Re-assign with a different version (should upsert, not error).
	body2 := `{"username":"alice","version":"2.0.0"}`
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/demo-skill/assignments", strings.NewReader(body2)))
	if rw.Code != http.StatusCreated {
		t.Fatalf("re-assign = %d body=%s", rw.Code, rw.Body.String())
	}

	// Unassign.
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("DELETE", "/api/skills/demo-skill/assignments/alice", nil))
	if rw.Code != http.StatusNoContent {
		t.Fatalf("unassign = %d body=%s", rw.Code, rw.Body.String())
	}

	// Post-unassign: alice no longer sees the skill.
	rig.userToInject = alice
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/demo-skill", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("post-unassign non-admin GET = %d, want 404", rw.Code)
	}
}

func TestSkillsHandlers_AssignNonAdminForbidden(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "restricted",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rig.regularUser(t, "alice")
	rig.userToInject = rig.regularUser(t, "bob")

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/demo-skill/assignments",
		strings.NewReader(`{"username":"alice"}`)))
	if rw.Code != http.StatusForbidden {
		t.Errorf("non-admin assign = %d, want 403", rw.Code)
	}

	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("DELETE", "/api/skills/demo-skill/assignments/alice", nil))
	if rw.Code != http.StatusForbidden {
		t.Errorf("non-admin unassign = %d, want 403", rw.Code)
	}
}

func TestSkillsHandlers_AssignRejectsUnknownUser(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "restricted",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/demo-skill/assignments",
		strings.NewReader(`{"username":"nobody-such-user"}`)))
	if rw.Code != http.StatusNotFound {
		t.Errorf("assign unknown user = %d, want 404", rw.Code)
	}
}

func TestSkillsHandlers_DownloadArchiveContents(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	original := makeArchive(t, skillMD)
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: original, Visibility: "public",
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET",
		"/api/skills/demo-skill/versions/1.0.0/archive", nil))
	got := readGzipFirstFile(t, rw.Body.Bytes())
	if !strings.HasPrefix(got, "SKILL.md:") {
		t.Errorf("archive first file = %q", got)
	}
}

// uploadResp matches the wire shape returned by uploadVersion: skills.UploadResult
// embedded plus an upstream_recorded bool added by the handler.
type uploadResp struct {
	skills.UploadResult
	UpstreamRecorded bool `json:"upstream_recorded"`
}

// pushVersion is a small helper for the upstream-related tests: builds an
// archive for `slug` at `version`, applies any caller-supplied headers, and
// returns the parsed response after asserting 201.
func pushVersion(t *testing.T, rig *skillsRig, slug, version, frontmatter string, headers map[string]string) *uploadResp {
	t.Helper()
	md := frontmatter
	if md == "" {
		md = strings.Replace(skillMD, "demo-skill", slug, 1)
	}
	archive := makeArchive(t, md)
	req := httptest.NewRequest("POST",
		"/api/skills/"+slug+"/versions/"+version+"?visibility=public",
		bytes.NewReader(archive))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusCreated {
		t.Fatalf("push %s@%s = %d body=%s", slug, version, rw.Code, rw.Body.String())
	}
	var resp uploadResp
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return &resp
}

// TestSkillsHandlers_UploadWithUpstreamHeader: push with X-Upstream creates a
// new upstream row and reports upstream_recorded=true.
func TestSkillsHandlers_UploadWithUpstreamHeader(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	resp := pushVersion(t, rig, "demo-skill", "1.0.0", "", map[string]string{
		"X-Upstream": `{"type":"git","url":"https://github.com/example/repo","subpath":"skills/demo","ref":"main"}`,
	})
	if !resp.UpstreamRecorded {
		t.Fatalf("upstream_recorded=false, want true; resp=%+v", resp)
	}

	u, err := rig.store.GetUpstream(resp.Skill.ID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if u == nil {
		t.Fatal("expected upstream row, got nil")
	}
	if u.GitURL != "https://github.com/example/repo" {
		t.Errorf("GitURL = %q", u.GitURL)
	}
	if u.GitSubpath != "skills/demo" {
		t.Errorf("GitSubpath = %q", u.GitSubpath)
	}
	if u.GitRef != "main" {
		t.Errorf("GitRef = %q", u.GitRef)
	}
	if u.UpstreamType != "git" {
		t.Errorf("UpstreamType = %q", u.UpstreamType)
	}
}

// TestSkillsHandlers_UploadPreservesExistingUpstreamAndClearsDrift: push without
// any upstream header leaves an existing row in place AND clears drift fields.
func TestSkillsHandlers_UploadPreservesExistingUpstreamAndClearsDrift(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	// First push with metadata to create the row.
	first := pushVersion(t, rig, "demo-skill", "1.0.0", "", map[string]string{
		"X-Upstream": `{"type":"git","url":"https://github.com/example/repo","ref":"main"}`,
	})
	skillID := first.Skill.ID

	// Seed a drift report so we can assert it gets cleared.
	if err := rig.store.WriteDriftReport(skillID, &store.DriftReport{
		RelayVersion:      "1.0.0",
		RelayHash:         "relayhash",
		UpstreamSHA:       "abc",
		UpstreamHash:      "upstreamhash",
		CommitsAhead:      2,
		Severity:          "minor",
		Summary:           "minor change upstream",
		RecommendedAction: "consider pulling",
		LLMModel:          "test",
		DetectedAt:        time.Now(),
	}); err != nil {
		t.Fatalf("seed drift: %v", err)
	}

	// Confirm drift was actually persisted before the next push.
	pre, err := rig.store.GetUpstream(skillID)
	if err != nil || pre == nil {
		t.Fatalf("GetUpstream pre: u=%v err=%v", pre, err)
	}
	if pre.DriftDetectedAt == nil {
		t.Fatal("expected DriftDetectedAt to be set after WriteDriftReport")
	}

	// Second push with NO upstream headers. Row should survive; drift should clear.
	resp := pushVersion(t, rig, "demo-skill", "1.1.0", "", nil)
	if resp.UpstreamRecorded {
		t.Errorf("upstream_recorded=true on no-metadata push, want false")
	}

	post, err := rig.store.GetUpstream(skillID)
	if err != nil {
		t.Fatalf("GetUpstream post: %v", err)
	}
	if post == nil {
		t.Fatal("upstream row was deleted by no-metadata push, want preserved")
	}
	if post.GitURL != "https://github.com/example/repo" {
		t.Errorf("GitURL changed: %q", post.GitURL)
	}
	if post.DriftDetectedAt != nil {
		t.Errorf("DriftDetectedAt should be nil after clear, got %v", post.DriftDetectedAt)
	}
	if post.DriftSeverity != nil {
		t.Errorf("DriftSeverity should be nil after clear, got %v", post.DriftSeverity)
	}
	// Phase 4 Task 11: ClearDriftReport now records the real subtree hash of
	// the just-uploaded archive as the new last_seen_hash baseline. Pre-Task
	// 11 this was passed as "" (empty placeholder); the test asserts the
	// upgrade landed.
	if post.LastSeenHash == nil || *post.LastSeenHash == "" {
		t.Errorf("LastSeenHash should be a real subhash digest after clear, got %v", post.LastSeenHash)
	}
	if post.LastSeenHash != nil && len(*post.LastSeenHash) != 64 {
		t.Errorf("LastSeenHash should be a 64-char sha256 hex, got %q (len=%d)",
			*post.LastSeenHash, len(*post.LastSeenHash))
	}
}

// TestSkillsHandlers_UploadClearUpstreamHeader: push with X-Clear-Upstream:true
// deletes the existing upstream row and reports upstream_recorded=false.
func TestSkillsHandlers_UploadClearUpstreamHeader(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	// Seed a row.
	first := pushVersion(t, rig, "demo-skill", "1.0.0", "", map[string]string{
		"X-Upstream": `{"type":"git","url":"https://github.com/example/repo"}`,
	})
	skillID := first.Skill.ID

	resp := pushVersion(t, rig, "demo-skill", "1.1.0", "", map[string]string{
		"X-Clear-Upstream": "true",
	})
	if resp.UpstreamRecorded {
		t.Errorf("upstream_recorded=true on clear, want false")
	}

	u, err := rig.store.GetUpstream(skillID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if u != nil {
		t.Errorf("upstream row should be deleted after clear, got %+v", u)
	}
}

// TestSkillsHandlers_UploadNoMetadataNoRow: push with no upstream metadata and
// no existing row → no-op on upstream side.
func TestSkillsHandlers_UploadNoMetadataNoRow(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	resp := pushVersion(t, rig, "demo-skill", "1.0.0", "", nil)
	if resp.UpstreamRecorded {
		t.Errorf("upstream_recorded=true with no metadata, want false")
	}

	u, err := rig.store.GetUpstream(resp.Skill.ID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if u != nil {
		t.Errorf("expected no upstream row, got %+v", u)
	}
}

// TestSkillsHandlers_UploadMalformedUpstreamHeader: malformed JSON or wrong
// type doesn't crash the handler and doesn't write a row; upstream_recorded=false.
func TestSkillsHandlers_UploadMalformedUpstreamHeader(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	// Malformed JSON.
	resp := pushVersion(t, rig, "demo-skill", "1.0.0", "", map[string]string{
		"X-Upstream": `not-json`,
	})
	if resp.UpstreamRecorded {
		t.Errorf("upstream_recorded=true on malformed JSON, want false")
	}
	u, err := rig.store.GetUpstream(resp.Skill.ID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if u != nil {
		t.Errorf("malformed JSON should not write a row, got %+v", u)
	}

	// Wrong type.
	rigB := newSkillsRig(t)
	rigB.userToInject = rigB.admin
	respB := pushVersion(t, rigB, "demo-skill", "1.0.0", "", map[string]string{
		"X-Upstream": `{"type":"svn","url":"https://example.com/repo"}`,
	})
	if respB.UpstreamRecorded {
		t.Errorf("upstream_recorded=true on type=svn, want false")
	}

	// Empty URL.
	rigC := newSkillsRig(t)
	rigC.userToInject = rigC.admin
	respC := pushVersion(t, rigC, "demo-skill", "1.0.0", "", map[string]string{
		"X-Upstream": `{"type":"git","url":""}`,
	})
	if respC.UpstreamRecorded {
		t.Errorf("upstream_recorded=true on empty url, want false")
	}
}

// ---------------------------------------------------------------------------
// POST /api/skills/{slug}/check-drift
// ---------------------------------------------------------------------------

// gitRunForTest runs `git <args>` in dir and fatals on error. Mirrors the
// helper in internal/skills/checker/git_test.go, duplicated here because Go
// doesn't share test helpers across packages.
func gitRunForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// makeFixtureRepo builds a fresh git repo in t.TempDir with one commit on
// main containing skills/foo/SKILL.md = "v1\n", and returns its path.
func makeFixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRunForTest(t, dir, "init", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, "skills", "foo"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "foo", "SKILL.md"), []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	gitRunForTest(t, dir, "add", ".")
	gitRunForTest(t, dir, "commit", "-m", "init")
	return dir
}

// commitFixtureUpdate writes the given content to skills/foo/SKILL.md and
// commits it. Used to introduce drift after baseline.
func commitFixtureUpdate(t *testing.T, repoDir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repoDir, "skills", "foo", "SKILL.md"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRunForTest(t, repoDir, "add", ".")
	gitRunForTest(t, repoDir, "commit", "-m", "update")
}

// resolveSrcHEAD clones src into a tempdir and returns origin/main's SHA.
// We don't compute a subpath hash because Detect's RevertedToSame branch
// only fires when both lastSeenHash != "" and newHash == lastSeenHash; an
// empty seeded hash means a genuine change still classifies as Drift, and
// a no-change check still classifies as NoMovement (sha-only short-circuit).
func resolveSrcHEAD(t *testing.T, gitURL string) string {
	t.Helper()
	tmpClone := filepath.Join(t.TempDir(), "prime-clone")
	if out, err := exec.Command("git", "clone", "--quiet", gitURL, tmpClone).CombinedOutput(); err != nil {
		t.Fatalf("prime clone: %v: %s", err, out)
	}
	out, err := exec.Command("git", "-C", tmpClone, "rev-parse", "origin/main").Output()
	if err != nil {
		t.Fatalf("prime rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// seedSkillWithUpstream creates a skill row + skill_upstreams row in one
// shot. lastSeenSHA may be empty for "first ever check"; the handler will
// then always classify as drift (matches Detect's documented semantics).
//
// UpsertUpstream's INSERT does not persist last_seen_sha (only last_seen_hash);
// to seed a steady-state baseline we follow it with UpdateUpstreamCheck which
// is the cron's normal write path for that column.
func seedSkillWithUpstream(t *testing.T, st *store.SkillStore, slug, gitURL, subpath, lastSeenSHA string) string {
	t.Helper()
	sk := &store.Skill{
		Slug:        slug,
		DisplayName: slug,
		Description: "fixture",
	}
	if err := st.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	u := &store.SkillUpstream{
		SkillID:    sk.ID,
		GitURL:     gitURL,
		GitSubpath: subpath,
		GitRef:     "origin/main",
	}
	if err := st.UpsertUpstream(u); err != nil {
		t.Fatalf("UpsertUpstream: %v", err)
	}
	if lastSeenSHA != "" {
		// UpsertUpstream doesn't accept last_seen_sha; bake the baseline in
		// via the same path the cron uses for no-drift outcomes.
		if err := st.UpdateUpstreamCheck(sk.ID, lastSeenSHA, "", time.Now().UTC()); err != nil {
			t.Fatalf("seed UpdateUpstreamCheck: %v", err)
		}
	}
	return sk.ID
}

// TestSkillsHandlers_CheckDrift_404Unknown: POST against an unknown slug → 404.
func TestSkillsHandlers_CheckDrift_404Unknown(t *testing.T) {
	rig := newSkillsRigWithCheckerEnabled(t)
	rig.userToInject = rig.admin

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/no-such-skill/check-drift", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("unknown slug = %d, want 404; body=%s", rw.Code, rw.Body.String())
	}
}

// TestSkillsHandlers_CheckDrift_409NoUpstream: skill exists but has no
// skill_upstreams row → 409.
func TestSkillsHandlers_CheckDrift_409NoUpstream(t *testing.T) {
	rig := newSkillsRigWithCheckerEnabled(t)
	rig.userToInject = rig.admin

	// Seed a skill via CreateSkill (no upstream row).
	if err := rig.store.CreateSkill(&store.Skill{
		Slug:        "no-upstream",
		DisplayName: "no-upstream",
		Description: "fixture",
	}); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/no-upstream/check-drift", nil))
	if rw.Code != http.StatusConflict {
		t.Errorf("no-upstream = %d, want 409; body=%s", rw.Code, rw.Body.String())
	}
}

// TestSkillsHandlers_CheckDrift_502FetchFailed: bogus git URL → 502.
// We point at a non-existent file:// path; EnsureCache's clone fails fast.
func TestSkillsHandlers_CheckDrift_502FetchFailed(t *testing.T) {
	rig := newSkillsRigWithCheckerEnabled(t)
	rig.userToInject = rig.admin

	bogusURL := "file://" + filepath.Join(t.TempDir(), "does-not-exist.git")
	seedSkillWithUpstream(t, rig.store, "fetch-fail", bogusURL, "", "")

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/fetch-fail/check-drift", nil))
	if rw.Code != http.StatusBadGateway {
		t.Errorf("fetch-fail = %d, want 502; body=%s", rw.Code, rw.Body.String())
	}
}

// TestSkillsHandlers_CheckDrift_204UpToDate: real fixture repo, baseline
// matches HEAD → 204.
func TestSkillsHandlers_CheckDrift_204UpToDate(t *testing.T) {
	rig := newSkillsRigWithCheckerEnabled(t)
	rig.userToInject = rig.admin

	src := makeFixtureRepo(t)
	gitURL := "file://" + src
	sha := resolveSrcHEAD(t, gitURL)
	seedSkillWithUpstream(t, rig.store, "still-skill", gitURL, "skills/foo", sha)

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/still-skill/check-drift", nil))
	if rw.Code != http.StatusNoContent {
		t.Errorf("up-to-date = %d, want 204; body=%s", rw.Code, rw.Body.String())
	}
}

// TestSkillsHandlers_CheckDrift_200Drift: real fixture repo with a new
// commit since baseline → 200 + drift JSON.
func TestSkillsHandlers_CheckDrift_200Drift(t *testing.T) {
	rig := newSkillsRigWithCheckerEnabled(t)
	rig.userToInject = rig.admin

	src := makeFixtureRepo(t)
	gitURL := "file://" + src
	sha := resolveSrcHEAD(t, gitURL)
	seedSkillWithUpstream(t, rig.store, "drift-skill", gitURL, "skills/foo", sha)

	// Introduce real drift after baseline.
	commitFixtureUpdate(t, src, "v2\nadded line\n")

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/drift-skill/check-drift", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("drift = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	var resp struct {
		Outdated bool           `json:"outdated"`
		Drift    map[string]any `json:"drift"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Outdated {
		t.Errorf("outdated = false, want true")
	}
	if resp.Drift == nil {
		t.Fatalf("drift block missing")
	}
	if _, ok := resp.Drift["detected_at"]; !ok {
		t.Errorf("drift.detected_at missing")
	}
	if _, ok := resp.Drift["upstream_sha"]; !ok {
		t.Errorf("drift.upstream_sha missing")
	}
	// LLM client is nil on this rig, so severity should be the deterministic
	// fallback "unknown".
	if got, _ := resp.Drift["severity"].(string); got != "unknown" {
		t.Errorf("drift.severity = %q, want %q", got, "unknown")
	}
}

// TestSkillsHandlers_CheckDrift_403NonAdmin: regular user → 403.
func TestSkillsHandlers_CheckDrift_403NonAdmin(t *testing.T) {
	rig := newSkillsRigWithCheckerEnabled(t)
	// Seed the skill so the 403 doesn't accidentally pass via 404. Note:
	// HandleSkillByPath returns 404 for non-admin GETs against restricted
	// skills, but the admin gate runs *after* the existence check for
	// other methods, so a missing slug here would 404 first. Pre-creating
	// the skill ensures we test the admin gate specifically.
	if err := rig.store.CreateSkill(&store.Skill{
		Slug:        "guarded",
		DisplayName: "guarded",
		Description: "fixture",
		Visibility:  "public",
	}); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	rig.userToInject = rig.regularUser(t, "alice")

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/guarded/check-drift", nil))
	if rw.Code != http.StatusForbidden {
		t.Errorf("non-admin = %d, want 403; body=%s", rw.Code, rw.Body.String())
	}
}

// TestSkillsHandlers_CheckDrift_405WrongMethod: GET → 405.
func TestSkillsHandlers_CheckDrift_405WrongMethod(t *testing.T) {
	rig := newSkillsRigWithCheckerEnabled(t)
	rig.userToInject = rig.admin
	if err := rig.store.CreateSkill(&store.Skill{
		Slug:        "method-test",
		DisplayName: "method-test",
		Description: "fixture",
		Visibility:  "public",
	}); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/method-test/check-drift", nil))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405; body=%s", rw.Code, rw.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Task 13: drift block on GET /api/skills and GET /api/skills/{slug}
// ---------------------------------------------------------------------------

// seedDriftedUpstream inserts an upstream row + writes a drift report so the
// skill flips Outdated=1 with the full drift_* field set populated. Returns
// the skill_id for follow-up assertions.
func seedDriftedUpstream(t *testing.T, st *store.SkillStore, slug string) string {
	t.Helper()
	sk, err := st.GetSkillBySlug(slug)
	if err != nil || sk == nil {
		t.Fatalf("seedDriftedUpstream: skill %q lookup failed: %v", slug, err)
	}
	if err := st.UpsertUpstream(&store.SkillUpstream{
		SkillID:      sk.ID,
		UpstreamType: "git",
		GitURL:       "https://github.com/example/repo",
		GitSubpath:   "skills/demo",
		GitRef:       "main",
	}); err != nil {
		t.Fatalf("seedDriftedUpstream: UpsertUpstream: %v", err)
	}
	if err := st.WriteDriftReport(sk.ID, &store.DriftReport{
		RelayVersion:      "1.0.0",
		RelayHash:         "relayhash-abcdef",
		UpstreamSHA:       "deadbeef0000000000000000000000000000beef",
		UpstreamHash:      "upstreamhash-abcdef",
		CommitsAhead:      3,
		Severity:          "major",
		Summary:           "upstream introduced new features",
		RecommendedAction: "review and pull",
		LLMModel:          "test-model",
		DetectedAt:        time.Now(),
	}); err != nil {
		t.Fatalf("seedDriftedUpstream: WriteDriftReport: %v", err)
	}
	return sk.ID
}

// TestSkillsHandlers_GetSkill_NoDrift: GET on a non-outdated skill should not
// surface a drift key in the JSON response (omitempty contract).
func TestSkillsHandlers_GetSkill_NoDrift(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "public",
	}); err != nil {
		t.Fatalf("seed upload: %v", err)
	}
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/demo-skill", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("GET = %d body=%s", rw.Code, rw.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rw.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["drift"]; ok {
		t.Errorf("non-outdated skill response should omit drift key, got %s", rw.Body.String())
	}
}

// TestSkillsHandlers_GetSkill_WithDrift: when Outdated=1 + a drift report is
// recorded, the GET response includes a populated drift block with the
// fields Task 14's CLI consumer will read.
func TestSkillsHandlers_GetSkill_WithDrift(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "public",
	}); err != nil {
		t.Fatalf("seed upload: %v", err)
	}
	seedDriftedUpstream(t, rig.store, "demo-skill")

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/demo-skill", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("GET = %d body=%s", rw.Code, rw.Body.String())
	}
	var resp struct {
		Skill *store.Skill   `json:"skill"`
		Drift map[string]any `json:"drift"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Drift == nil {
		t.Fatalf("drift block missing; body=%s", rw.Body.String())
	}
	for _, k := range []string{"severity", "summary", "recommended_action", "commits_ahead", "upstream_sha", "detected_at", "relay_version", "relay_hash", "llm_model"} {
		if _, ok := resp.Drift[k]; !ok {
			t.Errorf("drift.%s missing", k)
		}
	}
	if got, _ := resp.Drift["severity"].(string); got != "major" {
		t.Errorf("drift.severity = %q, want %q", got, "major")
	}
	if got, _ := resp.Drift["commits_ahead"].(float64); got != 3 {
		t.Errorf("drift.commits_ahead = %v, want 3", resp.Drift["commits_ahead"])
	}
}

// TestSkillsHandlers_ListSkills_MixedDrift: list endpoint with one outdated +
// one fresh skill returns drift only on the outdated row.
func TestSkillsHandlers_ListSkills_MixedDrift(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	// Outdated skill.
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "public",
	}); err != nil {
		t.Fatalf("seed outdated: %v", err)
	}
	seedDriftedUpstream(t, rig.store, "demo-skill")

	// Fresh skill (no upstream, no drift).
	freshMD := strings.Replace(skillMD, "demo-skill", "fresh-skill", 1)
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, freshMD), Visibility: "public",
	}); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("list = %d body=%s", rw.Code, rw.Body.String())
	}

	// Decode into a permissive shape so we can probe per-row drift presence.
	var resp struct {
		Skills []map[string]json.RawMessage `json:"skills"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Skills) != 2 {
		t.Fatalf("skills len = %d, want 2; body=%s", len(resp.Skills), rw.Body.String())
	}
	var sawOutdated, sawFresh bool
	for _, row := range resp.Skills {
		var slug string
		if err := json.Unmarshal(row["slug"], &slug); err != nil {
			t.Fatalf("decode slug: %v", err)
		}
		_, hasDrift := row["drift"]
		switch slug {
		case "demo-skill":
			sawOutdated = true
			if !hasDrift {
				t.Errorf("outdated skill missing drift key; row=%v", row)
			}
		case "fresh-skill":
			sawFresh = true
			if hasDrift {
				t.Errorf("fresh skill should not include drift key; row=%v", row)
			}
		}
	}
	if !sawOutdated || !sawFresh {
		t.Errorf("missing rows: outdated=%v fresh=%v", sawOutdated, sawFresh)
	}
}

// ---------------------------------------------------------------------------
// PUT/DELETE /api/skills/{slug}/upstream  (skill set-upstream design)
// ---------------------------------------------------------------------------

// seedSkillBySlug creates a bare skill row through the store so the upstream
// tests don't need a real archive — the upstream handler operates on the
// skill row directly, archives are irrelevant here.
func seedSkillBySlug(t *testing.T, st *store.SkillStore, slug string) *store.Skill {
	t.Helper()
	sk := &store.Skill{
		Slug:        slug,
		DisplayName: slug,
		Description: "fixture",
		Visibility:  "public",
	}
	if err := st.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	got, err := st.GetSkillBySlug(slug)
	if err != nil || got == nil {
		t.Fatalf("GetSkillBySlug after create: sk=%v err=%v", got, err)
	}
	return got
}

// TestSkillsHandlers_SetUpstream_PUTHappyPath: PUT on a skill with no row
// creates the row. Defaults type=git and ref=HEAD when caller omits them.
func TestSkillsHandlers_SetUpstream_PUTHappyPath(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	sk := seedSkillBySlug(t, rig.store, "track-me")

	body := `{"git_url":"https://github.com/example/repo","git_subpath":"skills/x"}`
	req := httptest.NewRequest("PUT", "/api/skills/track-me/upstream", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("PUT = %d body=%s", rw.Code, rw.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp["git_url"]; got != "https://github.com/example/repo" {
		t.Errorf("git_url = %v", got)
	}
	if got := resp["git_subpath"]; got != "skills/x" {
		t.Errorf("git_subpath = %v", got)
	}
	if got := resp["upstream_type"]; got != "git" {
		t.Errorf("upstream_type = %v, want git (default)", got)
	}
	if got := resp["git_ref"]; got != "HEAD" {
		t.Errorf("git_ref = %v, want HEAD (default)", got)
	}
	if resp["last_checked_at"] != nil {
		t.Errorf("last_checked_at = %v, want nil on freshly-set row", resp["last_checked_at"])
	}
	if resp["drift"] != nil {
		t.Errorf("drift = %v, want nil on freshly-set row", resp["drift"])
	}

	// Confirm the row landed in the store.
	u, err := rig.store.GetUpstream(sk.ID)
	if err != nil || u == nil {
		t.Fatalf("GetUpstream: u=%v err=%v", u, err)
	}
	if u.GitURL != "https://github.com/example/repo" {
		t.Errorf("stored GitURL = %q", u.GitURL)
	}
}

// TestSkillsHandlers_SetUpstream_PUTReplacePreservesDrift: PUT on a skill that
// already has an upstream row replaces the metadata fields (URL/path/ref) but
// preserves last_seen_* and drift_* — that's the documented "fix a typo'd
// pointer without losing prior check state" semantic.
func TestSkillsHandlers_SetUpstream_PUTReplacePreservesDrift(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	sk := seedSkillBySlug(t, rig.store, "replace-me")

	if err := rig.store.UpsertUpstream(&store.SkillUpstream{
		SkillID: sk.ID, GitURL: "https://github.com/old/repo", GitRef: "main",
	}); err != nil {
		t.Fatalf("seed UpsertUpstream: %v", err)
	}
	if err := rig.store.WriteDriftReport(sk.ID, &store.DriftReport{
		RelayVersion: "1.0.0", RelayHash: "rh", UpstreamSHA: "sha-deadbeef", UpstreamHash: "uh",
		CommitsAhead: 4, Severity: "minor", Summary: "x", RecommendedAction: "y",
		LLMModel: "test", DetectedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed WriteDriftReport: %v", err)
	}

	body := `{"git_url":"https://github.com/new/repo","git_ref":"feature-branch"}`
	req := httptest.NewRequest("PUT", "/api/skills/replace-me/upstream", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("PUT = %d body=%s", rw.Code, rw.Body.String())
	}

	post, err := rig.store.GetUpstream(sk.ID)
	if err != nil || post == nil {
		t.Fatalf("GetUpstream post: u=%v err=%v", post, err)
	}
	// Identity fields replaced.
	if post.GitURL != "https://github.com/new/repo" {
		t.Errorf("GitURL = %q, want https://github.com/new/repo", post.GitURL)
	}
	if post.GitRef != "feature-branch" {
		t.Errorf("GitRef = %q, want feature-branch", post.GitRef)
	}
	// Drift state preserved across the upsert (replacing the pointer doesn't
	// invalidate the prior check — the next checker run resolves it).
	if post.DriftSeverity == nil || *post.DriftSeverity != "minor" {
		t.Errorf("DriftSeverity = %v, want preserved 'minor'", post.DriftSeverity)
	}
	if post.DriftUpstreamSHA == nil || *post.DriftUpstreamSHA != "sha-deadbeef" {
		t.Errorf("DriftUpstreamSHA = %v, want preserved", post.DriftUpstreamSHA)
	}
}

// TestSkillsHandlers_SetUpstream_PUTMissingURL: empty git_url → 400.
func TestSkillsHandlers_SetUpstream_PUTMissingURL(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	seedSkillBySlug(t, rig.store, "missing-url")

	req := httptest.NewRequest("PUT", "/api/skills/missing-url/upstream",
		strings.NewReader(`{"git_url":""}`))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("missing url = %d, want 400; body=%s", rw.Code, rw.Body.String())
	}
}

// TestSkillsHandlers_SetUpstream_PUTUnsupportedType: non-git type pre-validated
// to 400 (not 500 from the CHECK constraint).
func TestSkillsHandlers_SetUpstream_PUTUnsupportedType(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	seedSkillBySlug(t, rig.store, "tarball-skill")

	req := httptest.NewRequest("PUT", "/api/skills/tarball-skill/upstream",
		strings.NewReader(`{"type":"tarball","git_url":"https://example.com/x.tar"}`))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("type=tarball = %d, want 400; body=%s", rw.Code, rw.Body.String())
	}
}

// TestSkillsHandlers_SetUpstream_PUTBadContentType: non-JSON content type → 415.
func TestSkillsHandlers_SetUpstream_PUTBadContentType(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	seedSkillBySlug(t, rig.store, "form-encoded")

	req := httptest.NewRequest("PUT", "/api/skills/form-encoded/upstream",
		strings.NewReader(`git_url=https://github.com/example/repo`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form-encoded = %d, want 415; body=%s", rw.Code, rw.Body.String())
	}
}

// TestSkillsHandlers_SetUpstream_PUTBadJSON: malformed JSON → 400.
func TestSkillsHandlers_SetUpstream_PUTBadJSON(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	seedSkillBySlug(t, rig.store, "bad-json")

	req := httptest.NewRequest("PUT", "/api/skills/bad-json/upstream",
		strings.NewReader(`not-json`))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON = %d, want 400; body=%s", rw.Code, rw.Body.String())
	}
}

// TestSkillsHandlers_SetUpstream_404Unknown: PUT/DELETE on a non-existent
// slug → 404. Slug existence is enforced by HandleSkillByPath before the
// upstream branch dispatches.
func TestSkillsHandlers_SetUpstream_404Unknown(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	for _, method := range []string{"PUT", "DELETE"} {
		req := httptest.NewRequest(method, "/api/skills/nope/upstream",
			strings.NewReader(`{"git_url":"https://github.com/example/repo"}`))
		req.Header.Set("Content-Type", "application/json")
		rw := httptest.NewRecorder()
		rig.mux.ServeHTTP(rw, req)
		if rw.Code != http.StatusNotFound {
			t.Errorf("%s nope = %d, want 404; body=%s", method, rw.Code, rw.Body.String())
		}
	}
}

// TestSkillsHandlers_SetUpstream_403NonAdminNoCap: non-admin user without the
// skills:write capability key → 403.
func TestSkillsHandlers_SetUpstream_403NonAdminNoCap(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.regularUser(t, "alice")
	seedSkillBySlug(t, rig.store, "guarded-up")

	req := httptest.NewRequest("PUT", "/api/skills/guarded-up/upstream",
		strings.NewReader(`{"git_url":"https://github.com/example/repo"}`))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Errorf("non-admin no-cap = %d, want 403; body=%s", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "skills:write") {
		t.Errorf("403 body should name the missing capability; got %s", rw.Body.String())
	}
}

// TestSkillsHandlers_SetUpstream_PassWithCapKey: non-admin user paired with an
// API key carrying skills:write succeeds. This is the CI-server path the
// design specifically calls out.
func TestSkillsHandlers_SetUpstream_PassWithCapKey(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.regularUser(t, "ci-user")
	rig.apiKeyToInject = &store.APIKey{Capabilities: []string{"skills:write"}}
	seedSkillBySlug(t, rig.store, "ci-skill")

	req := httptest.NewRequest("PUT", "/api/skills/ci-skill/upstream",
		strings.NewReader(`{"git_url":"https://github.com/example/repo"}`))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("cap key = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
}

// TestSkillsHandlers_SetUpstream_DELETE: DELETE removes the row and clears
// skills.outdated atomically.
func TestSkillsHandlers_SetUpstream_DELETE(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	sk := seedSkillBySlug(t, rig.store, "drop-me")

	if err := rig.store.UpsertUpstream(&store.SkillUpstream{
		SkillID: sk.ID, GitURL: "https://github.com/x/y", GitRef: "HEAD",
	}); err != nil {
		t.Fatalf("seed UpsertUpstream: %v", err)
	}
	if err := rig.store.WriteDriftReport(sk.ID, &store.DriftReport{
		RelayVersion: "1.0.0", RelayHash: "rh", UpstreamSHA: "sha", UpstreamHash: "uh",
		CommitsAhead: 1, Severity: "minor", Summary: "x", RecommendedAction: "y",
		LLMModel: "test", DetectedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed WriteDriftReport: %v", err)
	}

	req := httptest.NewRequest("DELETE", "/api/skills/drop-me/upstream", nil)
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("DELETE = %d body=%s", rw.Code, rw.Body.String())
	}

	u, err := rig.store.GetUpstream(sk.ID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if u != nil {
		t.Errorf("upstream row should be deleted, got %+v", u)
	}
	post, err := rig.store.GetSkill(sk.ID)
	if err != nil || post == nil {
		t.Fatalf("GetSkill: sk=%v err=%v", post, err)
	}
	if post.Outdated != 0 {
		t.Errorf("Outdated after DELETE = %d, want 0", post.Outdated)
	}
}

// TestSkillsHandlers_SetUpstream_405WrongMethod: GET/POST → 405.
func TestSkillsHandlers_SetUpstream_405WrongMethod(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	seedSkillBySlug(t, rig.store, "method-up")

	for _, method := range []string{"GET", "POST"} {
		req := httptest.NewRequest(method, "/api/skills/method-up/upstream", nil)
		rw := httptest.NewRecorder()
		rig.mux.ServeHTTP(rw, req)
		if rw.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405; body=%s", method, rw.Code, rw.Body.String())
		}
	}
}

// TestSkillsHandlers_CheckDrift_503Disabled: rig built without a checker →
// the handler reports 503. This is the production "cron disabled" path.
func TestSkillsHandlers_CheckDrift_503Disabled(t *testing.T) {
	rig := newSkillsRig(t) // no checker
	rig.userToInject = rig.admin
	if err := rig.store.CreateSkill(&store.Skill{
		Slug:        "disabled-test",
		DisplayName: "disabled-test",
		Description: "fixture",
		Visibility:  "public",
	}); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/disabled-test/check-drift", nil))
	if rw.Code != http.StatusServiceUnavailable {
		t.Errorf("disabled = %d, want 503; body=%s", rw.Code, rw.Body.String())
	}
}
