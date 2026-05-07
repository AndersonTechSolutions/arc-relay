package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/store"
)

// TestServersList_NotARedirect verifies that GET /servers as an admin returns
// 200 and not a redirect, even when the server list is empty.
func TestServersList_NotARedirect(t *testing.T) {
	rig := newDashRig(t)

	req := rig.adminReq(t, "GET", "/servers", nil)
	rw := httptest.NewRecorder()
	rig.h.handleServersList(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
}

// TestServersList_AdminSummaryAndTable seeds 4 servers with mixed statuses and
// verifies that the summary bar and table rows reflect the full set.
func TestServersList_AdminSummaryAndTable(t *testing.T) {
	rig := newDashRig(t)

	// Seed 4 servers: 2 running, 1 error, 1 stopped
	_ = seedServer(t, rig, "alpha-server", store.StatusRunning)
	_ = seedServer(t, rig, "beta-server", store.StatusRunning)
	_ = seedServer(t, rig, "gamma-server", store.StatusError)
	_ = seedServer(t, rig, "delta-server", store.StatusStopped)

	req := rig.adminReq(t, "GET", "/servers", nil)
	rw := httptest.NewRecorder()
	rig.h.handleServersList(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}

	body := rw.Body.String()

	// Summary stats — the template emits e.g. <strong>4</strong> total
	if !strings.Contains(body, ">4<") {
		t.Errorf("body missing '4 total'; body excerpt: %.500s", body)
	}
	if !strings.Contains(body, ">2<") {
		t.Errorf("body missing '2 running'")
	}
	if !strings.Contains(body, "errored") {
		t.Errorf("body missing 'errored' label")
	}
	if !strings.Contains(body, "stopped") {
		t.Errorf("body missing 'stopped' label")
	}
	if !strings.Contains(body, "running") {
		t.Errorf("body missing 'running' label")
	}

	// Verify each server's display name appears in the table
	for _, name := range []string{"alpha-server", "beta-server", "gamma-server", "delta-server"} {
		if !strings.Contains(body, name) {
			t.Errorf("body missing server display name %q", name)
		}
	}
}

// TestServersList_NonAdminScopedSummary seeds 3 running servers but grants the
// non-admin user profile access to only 1 of them; the summary must show "1 total".
func TestServersList_NonAdminScopedSummary(t *testing.T) {
	rig := newDashRig(t)

	srv1 := seedServer(t, rig, "pub-server", store.StatusRunning)
	srv2 := seedServer(t, rig, "priv-server-a", store.StatusRunning)
	srv3 := seedServer(t, rig, "priv-server-b", store.StatusRunning)
	_ = srv2
	_ = srv3

	// Create a non-admin user
	nonAdmin, err := rig.users.Create("nonadmin-user", "pw", "user")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Create a profile and grant access to only srv1.
	// endpoint_type must be one of 'tool', 'resource', 'prompt' per DB CHECK constraint.
	profileStore := store.NewProfileStore(rig.testDB)
	profile, err := profileStore.Create("test-profile", "")
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if err := profileStore.SetPermission(profile.ID, srv1.ID, "tool", "some-tool"); err != nil {
		t.Fatalf("SetPermission: %v", err)
	}

	// Link the profile to the non-admin user in the context used by handleServersList.
	nonAdmin.ProfileID = &profile.ID

	// Wire the profileStore into handlers so accessibleServers can resolve it.
	rig.h.profileStore = profileStore

	req := rig.userReq(t, nonAdmin, "GET", "/servers", nil)
	rw := httptest.NewRecorder()
	rig.h.handleServersList(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}

	body := rw.Body.String()

	// Should see "1 total" for the scoped user, not "3 total"
	if strings.Contains(body, ">3<") {
		t.Errorf("body should not contain 3 total; non-admin should see only 1 server")
	}
	if !strings.Contains(body, ">1<") {
		t.Errorf("body should contain 1 total for non-admin with 1 permitted server; body=%.800s", body)
	}

	// The accessible server's name should appear
	if !strings.Contains(body, "pub-server") {
		t.Errorf("body missing accessible server 'pub-server'")
	}
	// The inaccessible servers should NOT appear
	if strings.Contains(body, "priv-server-a") || strings.Contains(body, "priv-server-b") {
		t.Errorf("body should not contain inaccessible servers for non-admin user")
	}
}
