package web

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/comma-compliance/arc-relay/internal/store"
)

func newAPIFleetRig(t *testing.T) *dashRig {
	t.Helper()
	rig := newDashRig(t)
	// Wire a ProfileStore so handleAPIKeys doesn't panic on profileStore.List().
	rig.h.profileStore = store.NewProfileStore(rig.testDB)
	funcMap := template.FuncMap{
		"deref": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
		"timeAgo": timeAgoStr,
	}
	rig.h.tmpls["api_keys.html"] = template.Must(
		template.New("").Funcs(funcMap).
			ParseFS(templateFS, "templates/layout.html", "templates/api_keys.html"),
	)
	return rig
}

// TestAPIKeysFleet_AdminSeesAllRowsWithOwners verifies that GET /api-keys?all=1
// as admin returns 200 and shows all users' keys with Owner column.
func TestAPIKeysFleet_AdminSeesAllRowsWithOwners(t *testing.T) {
	rig := newAPIFleetRig(t)

	alice, err := rig.users.Create("fleet-alice", "pw", "user")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := rig.users.Create("fleet-bob", "pw", "user")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}

	if _, _, err := rig.users.CreateAPIKey(alice.ID, "alice-fleet-key", nil, nil); err != nil {
		t.Fatalf("CreateAPIKey alice: %v", err)
	}
	if _, _, err := rig.users.CreateAPIKey(bob.ID, "bob-fleet-key", nil, nil); err != nil {
		t.Fatalf("CreateAPIKey bob: %v", err)
	}

	req := rig.adminReq(t, "GET", "/api-keys?all=1", nil)
	req.URL.RawQuery = "all=1"
	rw := httptest.NewRecorder()
	rig.h.handleAPIKeys(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()

	for _, want := range []string{"fleet-alice", "fleet-bob", "dash-admin", "Owner"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestAPIKeysFleet_NonAdmin403 verifies that a non-admin user gets 403 when
// requesting fleet mode.
func TestAPIKeysFleet_NonAdmin403(t *testing.T) {
	rig := newAPIFleetRig(t)

	regularUser, err := rig.users.Create("fleet-regular", "pw", "user")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	req := rig.userReq(t, regularUser, "GET", "/api-keys?all=1", nil)
	req.URL.RawQuery = "all=1"
	rw := httptest.NewRecorder()
	rig.h.handleAPIKeys(rw, req)

	if rw.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rw.Code)
	}
}

// TestAPIKeysFleet_OrderRecentFirst verifies that fleet rows are ordered
// most-recently-used first, never-used last.
func TestAPIKeysFleet_OrderRecentFirst(t *testing.T) {
	rig := newAPIFleetRig(t)

	alice, err := rig.users.Create("order-alice", "pw", "user")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := rig.users.Create("order-bob", "pw", "user")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	never, err := rig.users.Create("order-never", "pw", "user")
	if err != nil {
		t.Fatalf("create never: %v", err)
	}

	_, kaAlice, err := rig.users.CreateAPIKey(alice.ID, "alice-recent-key", nil, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey alice: %v", err)
	}
	_, kaBob, err := rig.users.CreateAPIKey(bob.ID, "bob-older-key", nil, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey bob: %v", err)
	}
	if _, _, err := rig.users.CreateAPIKey(never.ID, "never-used-key", nil, nil); err != nil {
		t.Fatalf("CreateAPIKey never: %v", err)
	}

	// Set alice's last_used to now, bob's to 2 hours ago.
	now := time.Now()
	if _, err := rig.testDB.Exec("UPDATE api_keys SET last_used = ? WHERE id = ?", now, kaAlice.ID); err != nil {
		t.Fatalf("update alice last_used: %v", err)
	}
	if _, err := rig.testDB.Exec("UPDATE api_keys SET last_used = ? WHERE id = ?", now.Add(-2*time.Hour), kaBob.ID); err != nil {
		t.Fatalf("update bob last_used: %v", err)
	}

	req := rig.adminReq(t, "GET", "/api-keys?all=1", nil)
	req.URL.RawQuery = "all=1"
	rw := httptest.NewRecorder()
	rig.h.handleAPIKeys(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()

	alicePos := strings.Index(body, "order-alice")
	bobPos := strings.Index(body, "order-bob")
	neverPos := strings.Index(body, "never-used-key")

	if alicePos < 0 {
		t.Fatal("body missing order-alice")
	}
	if bobPos < 0 {
		t.Fatal("body missing order-bob")
	}
	if neverPos < 0 {
		t.Fatal("body missing never-used-key")
	}

	if alicePos >= bobPos {
		t.Errorf("alice (pos %d) should appear before bob (pos %d)", alicePos, bobPos)
	}
	if bobPos >= neverPos {
		t.Errorf("bob (pos %d) should appear before never-used (pos %d)", bobPos, neverPos)
	}
}

// TestAPIKeysFleet_NoCreateFormInFleetMode verifies that the "Generate New API
// Key" form is suppressed when in fleet mode.
func TestAPIKeysFleet_NoCreateFormInFleetMode(t *testing.T) {
	rig := newAPIFleetRig(t)

	req := rig.adminReq(t, "GET", "/api-keys?all=1", nil)
	req.URL.RawQuery = "all=1"
	rw := httptest.NewRecorder()
	rig.h.handleAPIKeys(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()

	if strings.Contains(body, "Generate New API Key") {
		t.Error("fleet mode body should NOT contain 'Generate New API Key'")
	}
}

// TestAPIKeysFleet_ToggleLink verifies that mine-mode shows "View all keys"
// link and fleet-mode shows "View only my keys" link.
func TestAPIKeysFleet_ToggleLink(t *testing.T) {
	rig := newAPIFleetRig(t)

	// Mine mode: should show "View all keys"
	req := rig.adminReq(t, "GET", "/api-keys", nil)
	rw := httptest.NewRecorder()
	rig.h.handleAPIKeys(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("mine-mode status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "View all keys") {
		t.Error("mine-mode body should contain 'View all keys'")
	}

	// Fleet mode: should show "View only my keys"
	req2 := rig.adminReq(t, "GET", "/api-keys?all=1", nil)
	req2.URL.RawQuery = "all=1"
	rw2 := httptest.NewRecorder()
	rig.h.handleAPIKeys(rw2, req2)

	if rw2.Code != http.StatusOK {
		t.Fatalf("fleet-mode status = %d, want 200; body=%s", rw2.Code, rw2.Body.String())
	}
	if !strings.Contains(rw2.Body.String(), "View only my keys") {
		t.Error("fleet-mode body should contain 'View only my keys'")
	}
}
