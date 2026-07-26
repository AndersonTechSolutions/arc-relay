package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/recipes"
	"github.com/comma-compliance/arc-relay/internal/server"
	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
	"github.com/comma-compliance/arc-relay/internal/web"
)

const validRecipeBody = `{"slug":"claude-mem","display_name":"Claude Mem","recipe_type":"claude_plugin","recipe_data":{"marketplace_source":"thedotmack/claude-mem","plugin":"claude-mem@thedotmack"},"visibility":"public"}`

type recipesRig struct {
	mux          *http.ServeMux
	store        *store.SetupRecipeStore
	svc          *recipes.Service
	users        *store.UserStore
	admin        *store.User
	userToInject *store.User
}

func newRecipesRig(t *testing.T) *recipesRig {
	t.Helper()
	db := testutil.OpenTestDB(t)
	st := store.NewSetupRecipeStore(db)
	svc := recipes.New(st)
	users := store.NewUserStore(db)

	admin, err := users.Create("rec-admin", "test-pw", "admin")
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	h := web.NewRecipesHandlers(svc, st, users,
		func(ctx context.Context) *store.User { return server.UserFromContext(ctx) },
		func(ctx context.Context) *store.APIKey { return server.APIKeyFromContext(ctx) },
	)

	rig := &recipesRig{store: st, svc: svc, users: users, admin: admin, mux: http.NewServeMux()}
	wrap := func(handler http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if rig.userToInject != nil {
				ctx = server.WithUser(ctx, rig.userToInject)
			}
			handler(w, r.WithContext(ctx))
		})
	}
	rig.mux.Handle("/api/recipes", wrap(h.HandleRecipes))
	rig.mux.Handle("/api/recipes/assigned", wrap(h.HandleAssigned))
	rig.mux.Handle("/api/recipes/", wrap(h.HandleRecipeByPath))
	return rig
}

func (r *recipesRig) regularUser(t *testing.T, username string) *store.User {
	t.Helper()
	u, err := r.users.Create(username, "test-pw", "user")
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return u
}

func TestRecipesHandlers_RequiresAuth(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = nil
	for _, path := range []string{"/api/recipes", "/api/recipes/assigned", "/api/recipes/foo"} {
		rw := httptest.NewRecorder()
		rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", path, nil))
		if rw.Code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", path, rw.Code)
		}
	}
}

func TestRecipesHandlers_CreateAdminOnly(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = rig.regularUser(t, "ian")

	req := httptest.NewRequest("POST", "/api/recipes", strings.NewReader(validRecipeBody))
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Errorf("non-admin POST = %d, want 403", rw.Code)
	}
}

func TestRecipesHandlers_CreateHappyPath(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = rig.admin

	req := httptest.NewRequest("POST", "/api/recipes", strings.NewReader(validRecipeBody))
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusCreated {
		t.Fatalf("create = %d body=%s", rw.Code, rw.Body.String())
	}
	var got store.SetupRecipe
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Slug != "claude-mem" || got.Visibility != "public" {
		t.Errorf("unexpected response: %+v", got)
	}
}

func TestRecipesHandlers_CreateRejectsInvalidPayload(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = rig.admin

	cases := []string{
		`{"slug":"x","recipe_type":"claude_plugin","recipe_data":{}}`,                                                      // missing fields
		`{"slug":"x","recipe_type":"shell_script","recipe_data":{"script":"echo"}}`,                                        // unsupported type
		`{"slug":"BAD","recipe_type":"claude_plugin","recipe_data":{"marketplace_source":"a/b","plugin":"x"}}`,             // bad slug
		`{"slug":"good-slug","recipe_type":"claude_plugin","recipe_data":{"marketplace_source":"a/b","plugin":"with sp"}}`, // bad plugin
	}
	for _, body := range cases {
		req := httptest.NewRequest("POST", "/api/recipes", strings.NewReader(body))
		rw := httptest.NewRecorder()
		rig.mux.ServeHTTP(rw, req)
		if rw.Code != http.StatusBadRequest {
			t.Errorf("body %q expected 400, got %d (%s)", body, rw.Code, rw.Body.String())
		}
	}
}

func TestRecipesHandlers_CreateOversize(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = rig.admin
	big := bytes.Repeat([]byte("x"), 65*1024)
	req := httptest.NewRequest("POST", "/api/recipes", bytes.NewReader(big))
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize = %d, want 413", rw.Code)
	}
}

func TestRecipesHandlers_DuplicateSlug(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = rig.admin
	for i, want := range []int{http.StatusCreated, http.StatusConflict} {
		req := httptest.NewRequest("POST", "/api/recipes", strings.NewReader(validRecipeBody))
		rw := httptest.NewRecorder()
		rig.mux.ServeHTTP(rw, req)
		if rw.Code != want {
			t.Errorf("attempt %d: got %d, want %d", i, rw.Code, want)
		}
	}
}

func TestRecipesHandlers_GetRecipe(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Create(&recipes.CreateInput{
		Slug: "claude-mem", RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`), Visibility: "public",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/recipes/claude-mem", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("GET = %d", rw.Code)
	}
	var got store.SetupRecipe
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Slug != "claude-mem" {
		t.Errorf("slug = %q", got.Slug)
	}
}

func TestRecipesHandlers_PatchPreservesData(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Create(&recipes.CreateInput{
		Slug: "patch-target", RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`), Visibility: "public",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	patch := `{"display_name":"Renamed","description":"new desc"}`
	req := httptest.NewRequest("PATCH", "/api/recipes/patch-target", strings.NewReader(patch))
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("PATCH = %d body=%s", rw.Code, rw.Body.String())
	}
	var got store.SetupRecipe
	_ = json.Unmarshal(rw.Body.Bytes(), &got)
	if got.DisplayName != "Renamed" {
		t.Errorf("DisplayName = %q", got.DisplayName)
	}
	// recipe_data preserved
	if !strings.Contains(string(got.RecipeData), `"plugin":"x"`) {
		t.Errorf("recipe_data lost: %s", got.RecipeData)
	}
}

func TestRecipesHandlers_AssignedFiltersByVisibility(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Create(&recipes.CreateInput{
		Slug: "shared", RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`), Visibility: "public",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.svc.Create(&recipes.CreateInput{
		Slug: "secret", RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"c/d","plugin":"y"}`), Visibility: "restricted",
	}); err != nil {
		t.Fatal(err)
	}

	rig.userToInject = rig.regularUser(t, "rec-bob")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/recipes/assigned", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("assigned = %d", rw.Code)
	}
	var resp struct {
		Assigned []*store.AssignedRecipe `json:"assigned"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &resp)
	if len(resp.Assigned) != 1 || resp.Assigned[0].Recipe.Slug != "shared" {
		t.Fatalf("expected only shared, got %+v", resp.Assigned)
	}
}

func TestRecipesHandlers_RegularUserCannotSeeRestrictedDirectly(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Create(&recipes.CreateInput{
		Slug: "secret-rec", RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"c/d","plugin":"y"}`), Visibility: "restricted",
	}); err != nil {
		t.Fatal(err)
	}
	rig.userToInject = rig.regularUser(t, "rec-outsider")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/recipes/secret-rec", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("non-admin direct GET on restricted = %d, want 404", rw.Code)
	}
}

func TestRecipesHandlers_DeleteAndHardDelete(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Create(&recipes.CreateInput{
		Slug: "dele", RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`), Visibility: "public",
	}); err != nil {
		t.Fatal(err)
	}

	// Soft-delete (yank) by default.
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("DELETE", "/api/recipes/dele", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("yank = %d", rw.Code)
	}
	got, _ := rig.store.GetRecipeBySlug("dele")
	if got == nil || got.YankedAt == nil {
		t.Errorf("expected yanked, got %+v", got)
	}

	// Hard delete removes the row.
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("DELETE", "/api/recipes/dele?hard=true", nil))
	if rw.Code != http.StatusNoContent {
		t.Fatalf("hard delete = %d", rw.Code)
	}
	got, _ = rig.store.GetRecipeBySlug("dele")
	if got != nil {
		t.Errorf("expected gone, got %+v", got)
	}
}

func TestRecipesHandlers_AssignmentLifecycle(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = rig.admin

	// Restricted recipe — only admin sees by default.
	if _, err := rig.svc.Create(&recipes.CreateInput{
		Slug: "secret-tool", RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`),
		Visibility: "restricted",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	alice := rig.regularUser(t, "alice")

	// Pre-grant: alice can't see it.
	rig.userToInject = alice
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/recipes/secret-tool", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("pre-grant = %d, want 404", rw.Code)
	}

	// Admin grants.
	rig.userToInject = rig.admin
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/recipes/secret-tool/assignments",
		strings.NewReader(`{"username":"alice"}`)))
	if rw.Code != http.StatusCreated {
		t.Fatalf("assign = %d body=%s", rw.Code, rw.Body.String())
	}

	// Alice now sees it via direct GET and via /assigned.
	rig.userToInject = alice
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/recipes/secret-tool", nil))
	if rw.Code != http.StatusOK {
		t.Errorf("post-grant direct GET = %d, want 200", rw.Code)
	}
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/recipes/assigned", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("post-grant assigned = %d", rw.Code)
	}
	if !bytes.Contains(rw.Body.Bytes(), []byte("secret-tool")) {
		t.Errorf("/assigned missing secret-tool: %s", rw.Body.String())
	}

	// Admin lists assignments.
	rig.userToInject = rig.admin
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/recipes/secret-tool/assignments", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("list assignments = %d", rw.Code)
	}
	if !bytes.Contains(rw.Body.Bytes(), []byte(alice.ID)) {
		t.Errorf("list missing alice: %s", rw.Body.String())
	}

	// Unassign.
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("DELETE", "/api/recipes/secret-tool/assignments/alice", nil))
	if rw.Code != http.StatusNoContent {
		t.Fatalf("unassign = %d", rw.Code)
	}

	// Post-unassign alice loses access.
	rig.userToInject = alice
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/recipes/secret-tool", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("post-unassign = %d, want 404", rw.Code)
	}
}

func TestRecipesHandlers_AssignNonAdminForbidden(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Create(&recipes.CreateInput{
		Slug: "rec", RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`),
		Visibility: "restricted",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rig.regularUser(t, "alice")
	rig.userToInject = rig.regularUser(t, "bob")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/recipes/rec/assignments",
		strings.NewReader(`{"username":"alice"}`)))
	if rw.Code != http.StatusForbidden {
		t.Errorf("non-admin assign = %d, want 403", rw.Code)
	}
}

func TestRecipesHandlers_AssignedRouteNotShadowedByPath(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = rig.admin
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/recipes/assigned", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("assigned = %d body=%s", rw.Code, rw.Body.String())
	}
	if !bytes.Contains(rw.Body.Bytes(), []byte(`"assigned":`)) {
		t.Errorf("body did not match HandleAssigned shape: %s", rw.Body.String())
	}
}

func TestRecipesHandlers_AdminListSeesAll(t *testing.T) {
	rig := newRecipesRig(t)
	rig.userToInject = rig.admin
	for _, body := range []string{
		`{"slug":"r1","recipe_type":"claude_plugin","recipe_data":{"marketplace_source":"a/b","plugin":"x"},"visibility":"public"}`,
		`{"slug":"r2","recipe_type":"claude_plugin","recipe_data":{"marketplace_source":"c/d","plugin":"y"},"visibility":"restricted"}`,
	} {
		req := httptest.NewRequest("POST", "/api/recipes", strings.NewReader(body))
		rw := httptest.NewRecorder()
		rig.mux.ServeHTTP(rw, req)
		if rw.Code != http.StatusCreated {
			t.Fatalf("seed: %d %s", rw.Code, rw.Body.String())
		}
	}
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/recipes", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("list = %d", rw.Code)
	}
	var resp struct {
		Recipes []*store.SetupRecipe `json:"recipes"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &resp)
	if len(resp.Recipes) != 2 {
		t.Errorf("admin list = %d, want 2", len(resp.Recipes))
	}
}
