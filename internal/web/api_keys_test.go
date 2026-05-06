package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/comma-compliance/arc-relay/internal/store"
)

// TestSanitizeCapabilities is a pure unit test for the form-input filter.
// Anything not in store.SupportedCapabilities must be dropped, regardless of
// whether the store-level normalizer would also drop it — the allowlist here
// is a defense-in-depth gate that surfaces any forward-compat additions as a
// deliberate code change rather than silent passthrough.
func TestSanitizeCapabilities(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, nil},
		{"all-allowed", []string{"skills:write", "recipes:write"}, []string{"skills:write", "recipes:write"}},
		{"unknown-dropped", []string{"skills:write", "skills:nuke"}, []string{"skills:write"}},
		{"trim-spaces", []string{"  skills:write  "}, []string{"skills:write"}},
		{"empty-strings-dropped", []string{"", "skills:write", ""}, []string{"skills:write"}},
		{"all-unknown-returns-empty", []string{"foo", "bar"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeCapabilities(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("sanitizeCapabilities(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// apiKeyRig wires a minimal Handlers with userStore + sessionStore + csrf
// secret, plus an authenticated admin (or regular) user injected via session.
// Mirrors csrf_test.go's newTestHandlers but keeps the user store accessible
// so tests can read back created keys.
type apiKeyRig struct {
	h        *Handlers
	users    *store.UserStore
	sessions *store.SessionStore
	user     *store.User
	sid      string
	csrfTok  string
}

func newAPIKeyRig(t *testing.T, role string) *apiKeyRig {
	t.Helper()
	h, sessions, _ := newTestHandlers(t)
	// newTestHandlers seeds an admin "testuser"; we want a fresh user with the
	// requested role so role-gating tests are unambiguous.
	user, err := h.users.Create("apikey-rig-"+role, "pw", role)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	sid := "session-" + user.ID
	if err := sessions.Create(sid, user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &apiKeyRig{
		h:        h,
		users:    h.users,
		sessions: sessions,
		user:     user,
		sid:      sid,
		csrfTok:  h.csrfToken(sid),
	}
}

func (r *apiKeyRig) postNewKey(t *testing.T, form url.Values) *http.Response {
	t.Helper()
	form.Set("csrf_token", r.csrfTok)
	req := httptest.NewRequest(http.MethodPost, "/api-keys/new",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "session", Value: r.sid})
	rw := httptest.NewRecorder()
	// Drive through requireAuth so CSRF + session lookup actually run.
	r.h.requireAuth(r.h.handleAPIKeyRoutes)(rw, req)
	return rw.Result()
}

// TestAPIKeyForm_AdminGrantsCapabilities: admin posts capability checkboxes →
// the created key carries those capabilities verbatim.
func TestAPIKeyForm_AdminGrantsCapabilities(t *testing.T) {
	r := newAPIKeyRig(t, "admin")
	form := url.Values{
		"key_name":   {"ci-publisher"},
		"capability": {"skills:write", "recipes:write"},
	}
	resp := r.postNewKey(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}

	keys, err := r.users.ListAPIKeys(r.user.ID)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	got := keys[0].Capabilities
	if len(got) != 2 {
		t.Fatalf("Capabilities = %v, want [recipes:write skills:write]", got)
	}
	// store.normalizeCapabilities sorts alphabetically.
	if got[0] != "recipes:write" || got[1] != "skills:write" {
		t.Errorf("Capabilities = %v, want [recipes:write skills:write]", got)
	}
}

// TestAPIKeyForm_NonAdminCapabilitiesIgnored: a non-admin posting capability
// values gets a key with NO capabilities. Capabilities must be admin-issued
// or they'd let any user trivially elevate.
func TestAPIKeyForm_NonAdminCapabilitiesIgnored(t *testing.T) {
	r := newAPIKeyRig(t, "user")
	form := url.Values{
		"key_name":   {"sneaky"},
		"capability": {"skills:write"},
	}
	resp := r.postNewKey(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302; body should still create a (capability-free) key", resp.StatusCode)
	}

	keys, err := r.users.ListAPIKeys(r.user.ID)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if len(keys[0].Capabilities) != 0 {
		t.Errorf("non-admin should get 0 capabilities; got %v", keys[0].Capabilities)
	}
}

// TestAPIKeyForm_AdminNoCheckboxesNoCapabilities: admin who didn't tick any
// checkboxes gets a regular key. Admin role still bypasses requireCapability,
// so this is the recommended posture for an admin's day-to-day key.
func TestAPIKeyForm_AdminNoCheckboxesNoCapabilities(t *testing.T) {
	r := newAPIKeyRig(t, "admin")
	form := url.Values{"key_name": {"daily-driver"}}
	resp := r.postNewKey(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}

	keys, err := r.users.ListAPIKeys(r.user.ID)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 1 || len(keys[0].Capabilities) != 0 {
		t.Errorf("expected 1 key with no capabilities; got %+v", keys)
	}
}

// TestAPIKeyForm_AdminUnknownCapabilityDropped: admin posts an unknown
// capability string → the allowlist filters it before reaching the store.
func TestAPIKeyForm_AdminUnknownCapabilityDropped(t *testing.T) {
	r := newAPIKeyRig(t, "admin")
	form := url.Values{
		"key_name":   {"forward-compat"},
		"capability": {"skills:write", "skills:nuke"},
	}
	resp := r.postNewKey(t, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}

	keys, err := r.users.ListAPIKeys(r.user.ID)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	got := keys[0].Capabilities
	if len(got) != 1 || got[0] != "skills:write" {
		t.Errorf("Capabilities = %v, want [skills:write] only", got)
	}
}
