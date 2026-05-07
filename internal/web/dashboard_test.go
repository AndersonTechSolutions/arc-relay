package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/comma-compliance/arc-relay/internal/store"
)

// seedServer creates a minimal test server with the given slug and status.
func seedServer(t *testing.T, rig *dashRig, slug string, status store.ServerStatus) *store.Server {
	t.Helper()
	srv := &store.Server{
		Name:        slug,
		DisplayName: slug,
		ServerType:  store.ServerTypeHTTP,
		Config:      json.RawMessage(`{"url":"http://localhost"}`),
	}
	if err := rig.serverStore.Create(srv); err != nil {
		t.Fatalf("Create server %q: %v", slug, err)
	}
	if status != store.StatusStopped {
		if err := rig.serverStore.UpdateStatus(srv.ID, status, ""); err != nil {
			t.Fatalf("UpdateStatus %q: %v", slug, err)
		}
		srv.Status = status
	}
	return srv
}

// TestDashboardTiles_AdminCounts: admin sees all four tiles with correct data-tile attributes.
func TestDashboardTiles_AdminCounts(t *testing.T) {
	rig := newDashRig(t)

	// Seed 2 public skills + 1 restricted
	sk1 := &store.Skill{Slug: "pub-skill-1", DisplayName: "Pub 1", Description: "x", Visibility: "public"}
	sk2 := &store.Skill{Slug: "pub-skill-2", DisplayName: "Pub 2", Description: "x", Visibility: "public"}
	sk3 := &store.Skill{Slug: "restr-skill", DisplayName: "Restr", Description: "x", Visibility: "restricted"}
	for _, sk := range []*store.Skill{sk1, sk2, sk3} {
		if err := rig.store.CreateSkill(sk); err != nil {
			t.Fatalf("CreateSkill %q: %v", sk.Slug, err)
		}
	}

	// Seed 1 recipe
	rec := &store.SetupRecipe{
		Slug:        "test-recipe",
		DisplayName: "Test Recipe",
		Description: "y",
		RecipeType:  store.RecipeTypeClaudePlugin,
		Visibility:  "public",
		RecipeData:  json.RawMessage(`{}`),
	}
	if err := rig.recipeStore.CreateRecipe(rec); err != nil {
		t.Fatalf("CreateRecipe: %v", err)
	}

	// Seed 2 servers: 1 running, 1 stopped
	seedServer(t, rig, "srv-running", store.StatusRunning)
	seedServer(t, rig, "srv-stopped", store.StatusStopped)

	req := rig.adminReq(t, "GET", "/", nil)
	rw := httptest.NewRecorder()
	rig.h.handleDashboard(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()

	for _, attr := range []string{`data-tile="skills"`, `data-tile="recipes"`, `data-tile="servers"`, `data-tile="devices"`} {
		if !strings.Contains(body, attr) {
			t.Errorf("body missing %s", attr)
		}
	}
}

// TestDashboardTiles_NonAdminScopedAndDevicesHidden: non-admin lacks devices tile.
func TestDashboardTiles_NonAdminScopedAndDevicesHidden(t *testing.T) {
	rig := newDashRig(t)

	// Seed 1 public skill
	sk := &store.Skill{Slug: "pub-skill", DisplayName: "Pub", Description: "x", Visibility: "public"}
	if err := rig.store.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}

	// Create a non-admin user
	regularUser, err := rig.users.Create("regular-user", "pw", "user")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	req := rig.userReq(t, regularUser, "GET", "/", nil)
	rw := httptest.NewRecorder()
	rig.h.handleDashboard(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()

	if strings.Contains(body, `data-tile="devices"`) {
		t.Error("non-admin should NOT see devices tile, but it was present")
	}
	if !strings.Contains(body, `data-tile="skills"`) {
		t.Error("body missing data-tile=\"skills\"")
	}
}

// TestDashboardTiles_DeviceSubtextActiveLastHour: admin sees "2 active (last hour)".
func TestDashboardTiles_DeviceSubtextActiveLastHour(t *testing.T) {
	rig := newDashRig(t)

	// Create 5 API keys for admin (reuse admin user)
	for i := 0; i < 5; i++ {
		if _, _, err := rig.users.CreateAPIKey(rig.admin.ID, "key-"+string(rune('a'+i)), nil, nil); err != nil {
			t.Fatalf("CreateAPIKey: %v", err)
		}
	}

	// Mark 2 keys recently used (within last hour) and 2 used 2 hours ago,
	// 1 left with last_used IS NULL.
	// We use the raw DB to set last_used directly.
	rows, err := rig.testDB.Query("SELECT id FROM api_keys WHERE user_id = ? ORDER BY created_at", rig.admin.ID)
	if err != nil {
		t.Fatalf("query api_keys: %v", err)
	}
	var keyIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan key id: %v", err)
		}
		keyIDs = append(keyIDs, id)
	}
	_ = rows.Close()

	if len(keyIDs) < 5 {
		t.Fatalf("expected 5 keys, got %d", len(keyIDs))
	}

	recentTime := time.Now().Add(-30 * time.Minute)
	oldTime := time.Now().Add(-2 * time.Hour)

	// Keys 0 and 1: recently used
	for _, id := range keyIDs[:2] {
		if _, err := rig.testDB.Exec("UPDATE api_keys SET last_used = ? WHERE id = ?", recentTime, id); err != nil {
			t.Fatalf("update last_used (recent): %v", err)
		}
	}
	// Keys 2 and 3: used 2 hours ago
	for _, id := range keyIDs[2:4] {
		if _, err := rig.testDB.Exec("UPDATE api_keys SET last_used = ? WHERE id = ?", oldTime, id); err != nil {
			t.Fatalf("update last_used (old): %v", err)
		}
	}
	// Key 4: last_used remains NULL (no update needed)

	req := rig.adminReq(t, "GET", "/", nil)
	rw := httptest.NewRecorder()
	rig.h.handleDashboard(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()

	if !strings.Contains(body, "2 active (last hour)") {
		t.Errorf("body missing \"2 active (last hour)\"; got body excerpt around devices: %s",
			extractAround(body, "active"))
	}
}

// TestDashboard_TileLinks: tiles link to correct paths.
func TestDashboard_TileLinks(t *testing.T) {
	rig := newDashRig(t)

	req := rig.adminReq(t, "GET", "/", nil)
	rw := httptest.NewRecorder()
	rig.h.handleDashboard(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()

	for _, link := range []string{"/skills", "/recipes", "/servers", "/api-keys?all=1"} {
		if !strings.Contains(body, link) {
			t.Errorf("body missing link %q", link)
		}
	}
}

// extractAround returns up to 200 chars around the first occurrence of needle.
func extractAround(s, needle string) string {
	idx := strings.Index(s, needle)
	if idx < 0 {
		return "(not found)"
	}
	start := idx - 100
	if start < 0 {
		start = 0
	}
	end := idx + 100
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}
