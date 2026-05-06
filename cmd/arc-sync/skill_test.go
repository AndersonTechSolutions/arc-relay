package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/cli/relay"
)

// fakeDriftClient stubs the driftClient interface so we can drive
// checkUpdates without a real httptest server. Each method is keyed off the
// slug; ListSkills returns the configured slug list.
type fakeDriftClient struct {
	skills []*relay.Skill
	drift  map[string]*relay.DriftBlock
	errors map[string]error
}

func (f *fakeDriftClient) CheckDrift(slug string) (*relay.DriftBlock, error) {
	if err, ok := f.errors[slug]; ok {
		return nil, err
	}
	return f.drift[slug], nil
}

func (f *fakeDriftClient) ListSkills() ([]*relay.Skill, error) {
	return f.skills, nil
}

func TestCheckUpdates_SingleSkill_Drift(t *testing.T) {
	c := &fakeDriftClient{
		drift: map[string]*relay.DriftBlock{
			"foo": {
				Severity:          "minor",
				Summary:           "3 commits behind",
				RecommendedAction: "run `arc-sync skill push`",
			},
		},
	}
	var stdout, stderr bytes.Buffer
	if err := checkUpdates(c, "foo", &stdout, &stderr); err != nil {
		t.Fatalf("checkUpdates: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "outdated · minor: 3 commits behind") {
		t.Errorf("stdout missing summary line: %q", out)
	}
	if !strings.Contains(out, "run `arc-sync skill push`") {
		t.Errorf("stdout missing recommended action: %q", out)
	}
}

func TestCheckUpdates_SingleSkill_UpToDate(t *testing.T) {
	c := &fakeDriftClient{
		drift: map[string]*relay.DriftBlock{"foo": nil},
	}
	var stdout, stderr bytes.Buffer
	if err := checkUpdates(c, "foo", &stdout, &stderr); err != nil {
		t.Fatalf("checkUpdates: %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "up-to-date" {
		t.Errorf("stdout = %q, want %q", got, "up-to-date")
	}
}

func TestCheckUpdates_SingleSkill_NotFound(t *testing.T) {
	c := &fakeDriftClient{
		errors: map[string]error{
			"missing": &relay.SkillHTTPError{Status: http.StatusNotFound},
		},
	}
	var stdout, stderr bytes.Buffer
	err := checkUpdates(c, "missing", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(stderr.String(), "skill not found") {
		t.Errorf("stderr missing 'skill not found': %q", stderr.String())
	}
}

func TestCheckUpdates_SingleSkill_NoUpstream(t *testing.T) {
	c := &fakeDriftClient{
		errors: map[string]error{
			"foo": &relay.SkillHTTPError{Status: http.StatusConflict},
		},
	}
	var stdout, stderr bytes.Buffer
	err := checkUpdates(c, "foo", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for 409 in single-skill mode")
	}
	if !strings.Contains(stderr.String(), "no upstream tracking configured") {
		t.Errorf("stderr missing 409 message: %q", stderr.String())
	}
}

func TestCheckUpdates_SingleSkill_UpstreamFailed(t *testing.T) {
	c := &fakeDriftClient{
		errors: map[string]error{
			"foo": &relay.SkillHTTPError{Status: http.StatusBadGateway},
		},
	}
	var stdout, stderr bytes.Buffer
	err := checkUpdates(c, "foo", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for 502")
	}
	if !strings.Contains(stderr.String(), "upstream fetch failed") {
		t.Errorf("stderr missing 502 message: %q", stderr.String())
	}
}

// TestCheckUpdates_AllSkills exercises iteration mode: 409s skipped silently,
// 204s print up-to-date, drift hits print "outdated · <severity>".
func TestCheckUpdates_AllSkills(t *testing.T) {
	c := &fakeDriftClient{
		skills: []*relay.Skill{
			{Slug: "alpha"},
			{Slug: "beta"},
			{Slug: "gamma"},
		},
		drift: map[string]*relay.DriftBlock{
			"alpha": {Severity: "minor", Summary: "x"},
			// beta has no entry → returns nil = up-to-date
		},
		errors: map[string]error{
			"gamma": &relay.SkillHTTPError{Status: http.StatusConflict},
		},
	}
	var stdout, stderr bytes.Buffer
	if err := checkUpdates(c, "", &stdout, &stderr); err != nil {
		t.Fatalf("checkUpdates: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "alpha: outdated · minor") {
		t.Errorf("stdout missing alpha drift line: %q", out)
	}
	if !strings.Contains(out, "beta: up-to-date") {
		t.Errorf("stdout missing beta up-to-date line: %q", out)
	}
	// gamma should be silently skipped — no line in stdout, nothing in stderr.
	if strings.Contains(out, "gamma") {
		t.Errorf("gamma should be silently skipped on 409, got: %q", out)
	}
	if strings.Contains(stderr.String(), "gamma") {
		t.Errorf("gamma should not appear in stderr on 409, got: %q", stderr.String())
	}
}

// fakeUpstreamClient stubs upstreamClient so the set/clear tests don't have
// to spin a real httptest server. Each method records its arguments so we
// can assert the CLI passed the right values.
type fakeUpstreamClient struct {
	setSlug  string
	setIn    *relay.SetUpstreamInput
	setResp  map[string]any
	setErr   error
	clearArg string
	clearErr error
}

func (f *fakeUpstreamClient) SetUpstream(slug string, in *relay.SetUpstreamInput) (map[string]any, error) {
	f.setSlug = slug
	f.setIn = in
	return f.setResp, f.setErr
}

func (f *fakeUpstreamClient) ClearUpstream(slug string) error {
	f.clearArg = slug
	return f.clearErr
}

func TestSetUpstream_HappyPath(t *testing.T) {
	c := &fakeUpstreamClient{
		setResp: map[string]any{
			"git_url":     "https://github.com/example/repo",
			"git_subpath": "skills/foo",
			"git_ref":     "main",
		},
	}
	var stdout, stderr bytes.Buffer
	if err := setUpstream(c, "foo", "https://github.com/example/repo", "skills/foo", "main", &stdout, &stderr); err != nil {
		t.Fatalf("setUpstream: %v", err)
	}
	if c.setSlug != "foo" {
		t.Errorf("slug arg = %q", c.setSlug)
	}
	if c.setIn == nil || c.setIn.GitURL != "https://github.com/example/repo" {
		t.Errorf("SetUpstreamInput = %+v", c.setIn)
	}
	if c.setIn.Subpath != "skills/foo" || c.setIn.Ref != "main" {
		t.Errorf("SetUpstreamInput subpath/ref = %+v", c.setIn)
	}
	out := stdout.String()
	if !strings.Contains(out, "Set upstream for foo") {
		t.Errorf("stdout missing summary line: %q", out)
	}
	if !strings.Contains(out, "https://github.com/example/repo @ main") {
		t.Errorf("stdout missing url+ref: %q", out)
	}
	if !strings.Contains(out, "path=skills/foo") {
		t.Errorf("stdout missing path: %q", out)
	}
}

func TestSetUpstream_DefaultsRepoRoot(t *testing.T) {
	c := &fakeUpstreamClient{
		setResp: map[string]any{
			"git_url":     "https://github.com/example/repo",
			"git_subpath": "",
			"git_ref":     "HEAD",
		},
	}
	var stdout, stderr bytes.Buffer
	if err := setUpstream(c, "foo", "https://github.com/example/repo", "", "", &stdout, &stderr); err != nil {
		t.Fatalf("setUpstream: %v", err)
	}
	if !strings.Contains(stdout.String(), "path=(repo root)") {
		t.Errorf("stdout should label empty subpath as (repo root): %q", stdout.String())
	}
}

func TestSetUpstream_404(t *testing.T) {
	c := &fakeUpstreamClient{
		setErr: &relay.SkillHTTPError{Status: http.StatusNotFound},
	}
	var stdout, stderr bytes.Buffer
	err := setUpstream(c, "ghost", "https://github.com/example/repo", "", "", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(stderr.String(), "ghost: not found on relay") {
		t.Errorf("stderr missing 404 message: %q", stderr.String())
	}
}

func TestSetUpstream_403(t *testing.T) {
	c := &fakeUpstreamClient{
		setErr: &relay.SkillHTTPError{Status: http.StatusForbidden},
	}
	var stdout, stderr bytes.Buffer
	err := setUpstream(c, "guarded", "https://github.com/example/repo", "", "", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(stderr.String(), "admin role or skills:write API key required") {
		t.Errorf("stderr missing 403 hint: %q", stderr.String())
	}
}

func TestSetUpstream_400(t *testing.T) {
	c := &fakeUpstreamClient{
		setErr: &relay.SkillHTTPError{Status: http.StatusBadRequest},
	}
	var stdout, stderr bytes.Buffer
	err := setUpstream(c, "foo", "", "", "", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !strings.Contains(stderr.String(), "bad request") {
		t.Errorf("stderr missing 400 message: %q", stderr.String())
	}
}

func TestClearUpstream_HappyPath(t *testing.T) {
	c := &fakeUpstreamClient{}
	var stdout, stderr bytes.Buffer
	if err := clearUpstream(c, "foo", &stdout, &stderr); err != nil {
		t.Fatalf("clearUpstream: %v", err)
	}
	if c.clearArg != "foo" {
		t.Errorf("slug arg = %q", c.clearArg)
	}
	if !strings.Contains(stdout.String(), "Cleared upstream tracking for foo") {
		t.Errorf("stdout missing confirmation: %q", stdout.String())
	}
}

// fakeEditClient stubs editClient for the `skill edit` tests.
type fakeEditClient struct {
	gotSlug string
	gotIn   *relay.PatchSkillInput
	resp    *relay.Skill
	err     error
}

func (f *fakeEditClient) PatchSkill(slug string, in *relay.PatchSkillInput) (*relay.Skill, error) {
	f.gotSlug = slug
	f.gotIn = in
	return f.resp, f.err
}

func TestEditSkill_VisibilityFlip(t *testing.T) {
	c := &fakeEditClient{
		resp: &relay.Skill{Slug: "foo", Visibility: "public", DisplayName: "Foo"},
	}
	v := "public"
	in := &relay.PatchSkillInput{Visibility: &v}

	var stdout, stderr bytes.Buffer
	if err := editSkill(c, "foo", in, &stdout, &stderr); err != nil {
		t.Fatalf("editSkill: %v", err)
	}
	if c.gotSlug != "foo" {
		t.Errorf("slug arg = %q", c.gotSlug)
	}
	if c.gotIn == nil || c.gotIn.Visibility == nil || *c.gotIn.Visibility != "public" {
		t.Errorf("PatchSkillInput.Visibility = %+v", c.gotIn)
	}
	if !strings.Contains(stdout.String(), "Updated foo: visibility=public") {
		t.Errorf("stdout missing summary: %q", stdout.String())
	}
}

func TestEditSkill_DisplayName(t *testing.T) {
	c := &fakeEditClient{
		resp: &relay.Skill{Slug: "foo", Visibility: "restricted", DisplayName: "Renamed"},
	}
	dn := "Renamed"
	in := &relay.PatchSkillInput{DisplayName: &dn}

	var stdout, stderr bytes.Buffer
	if err := editSkill(c, "foo", in, &stdout, &stderr); err != nil {
		t.Fatalf("editSkill: %v", err)
	}
	if !strings.Contains(stdout.String(), `display="Renamed"`) {
		t.Errorf("stdout missing display rename: %q", stdout.String())
	}
}

func TestEditSkill_400(t *testing.T) {
	c := &fakeEditClient{err: &relay.SkillHTTPError{Status: http.StatusBadRequest}}
	v := "halfway"
	in := &relay.PatchSkillInput{Visibility: &v}

	var stdout, stderr bytes.Buffer
	err := editSkill(c, "foo", in, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !strings.Contains(stderr.String(), "bad request") {
		t.Errorf("stderr missing 400 message: %q", stderr.String())
	}
}

func TestEditSkill_404(t *testing.T) {
	c := &fakeEditClient{err: &relay.SkillHTTPError{Status: http.StatusNotFound}}
	v := "public"
	in := &relay.PatchSkillInput{Visibility: &v}

	var stdout, stderr bytes.Buffer
	err := editSkill(c, "ghost", in, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(stderr.String(), "ghost: not found on relay") {
		t.Errorf("stderr missing 404 message: %q", stderr.String())
	}
}

func TestEditSkill_403(t *testing.T) {
	c := &fakeEditClient{err: &relay.SkillHTTPError{Status: http.StatusForbidden}}
	v := "public"
	in := &relay.PatchSkillInput{Visibility: &v}

	var stdout, stderr bytes.Buffer
	err := editSkill(c, "foo", in, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(stderr.String(), "admin role or skills:write API key required") {
		t.Errorf("stderr missing 403 hint: %q", stderr.String())
	}
}

func TestClearUpstream_404(t *testing.T) {
	c := &fakeUpstreamClient{
		clearErr: &relay.SkillHTTPError{Status: http.StatusNotFound},
	}
	var stdout, stderr bytes.Buffer
	err := clearUpstream(c, "ghost", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(stderr.String(), "ghost: not found on relay") {
		t.Errorf("stderr missing 404 message: %q", stderr.String())
	}
}

// TestPrintSkillUsage_MentionsCheckUpdates is a smoke test that the help
// output advertises the new subcommand. It also indirectly verifies that
// the dispatcher's `case "check-updates":` arm is discoverable to users.
// Direct coverage of runSkill (which reads os.Args + builds a real config)
// is out of scope for a unit test.
func TestPrintSkillUsage_MentionsCheckUpdates(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	printSkillUsage()
	_ = w.Close()
	os.Stdout = saved

	var captured bytes.Buffer
	if _, err := io.Copy(&captured, r); err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if !strings.Contains(captured.String(), "check-updates") {
		t.Errorf("printSkillUsage output missing check-updates: %q", captured.String())
	}
	if !strings.Contains(captured.String(), "set-upstream") {
		t.Errorf("printSkillUsage output missing set-upstream: %q", captured.String())
	}
	if !strings.Contains(captured.String(), "clear-upstream") {
		t.Errorf("printSkillUsage output missing clear-upstream: %q", captured.String())
	}
	if !strings.Contains(captured.String(), "edit <slug>") {
		t.Errorf("printSkillUsage output missing edit subcommand: %q", captured.String())
	}
}
