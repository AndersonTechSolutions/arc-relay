package recipes_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/recipes"
	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
)

func newService(t *testing.T) (*recipes.Service, *store.SetupRecipeStore) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	st := store.NewSetupRecipeStore(db)
	return recipes.New(st), st
}

func TestValidateRecipeData_HappyPath(t *testing.T) {
	out, err := recipes.ValidateRecipeData(store.RecipeTypeClaudePlugin,
		json.RawMessage(`{"marketplace_source":"thedotmack/claude-mem","plugin":"claude-mem@thedotmack"}`))
	if err != nil {
		t.Fatalf("ValidateRecipeData: %v", err)
	}
	var got recipes.ClaudePluginRecipe
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.MarketplaceSource != "thedotmack/claude-mem" {
		t.Errorf("MarketplaceSource = %q", got.MarketplaceSource)
	}
	if !got.Enabled {
		t.Error("Enabled should default to true when omitted")
	}
}

func TestValidateRecipeData_RejectsUnknownType(t *testing.T) {
	_, err := recipes.ValidateRecipeData("shell_script", json.RawMessage(`{"script":"echo"}`))
	if !errors.Is(err, recipes.ErrInvalidRecipe) {
		t.Fatalf("expected ErrInvalidRecipe for unknown type, got %v", err)
	}
}

func TestValidateRecipeData_RequiredFields(t *testing.T) {
	cases := []string{
		`{}`,
		`{"plugin":"x"}`,                         // no marketplace_source
		`{"marketplace_source":"a/b"}`,           // no plugin
		`{"marketplace_source":"","plugin":"x"}`, // empty marketplace
	}
	for _, c := range cases {
		_, err := recipes.ValidateRecipeData(store.RecipeTypeClaudePlugin, json.RawMessage(c))
		if !errors.Is(err, recipes.ErrInvalidRecipe) {
			t.Errorf("expected ErrInvalidRecipe for %q, got %v", c, err)
		}
	}
}

func TestValidateRecipeData_AcceptedSourceForms(t *testing.T) {
	cases := []string{
		"thedotmack/claude-mem",             // github owner/repo
		"https://github.com/X/Y",            // https URL
		"https://github.com/X/Y.git",        // .git suffix
		"git@github.com:X/Y.git",            // ssh form
		"git://example.com/X.git",           // git protocol
		"/Users/ian/code/local-marketplace", // absolute path
		"file:///Users/ian/code/local",      // file URL
	}
	for _, src := range cases {
		body := `{"marketplace_source":` + jsonString(src) + `,"plugin":"x"}`
		_, err := recipes.ValidateRecipeData(store.RecipeTypeClaudePlugin, json.RawMessage(body))
		if err != nil {
			t.Errorf("source %q rejected: %v", src, err)
		}
	}
}

func TestValidateRecipeData_RejectedSourceForms(t *testing.T) {
	cases := []string{
		"justrepo",            // no slash
		"too/many/slashes",    // multiple slashes
		"with space/repo",     // whitespace
		"-leadinghyphen/repo", // bad ident
		"repo/",               // empty second component
		"/justone",            // path-prefix BUT also fails owner/repo, accepted via "starts with /" branch — wait this is actually accepted
	}
	// "/justone" is accepted via the absolute-path branch — remove from rejected list.
	rejectable := cases[:5]
	for _, src := range rejectable {
		body := `{"marketplace_source":` + jsonString(src) + `,"plugin":"x"}`
		_, err := recipes.ValidateRecipeData(store.RecipeTypeClaudePlugin, json.RawMessage(body))
		if !errors.Is(err, recipes.ErrInvalidRecipe) {
			t.Errorf("source %q should be rejected, got err=%v", src, err)
		}
	}
}

func TestValidateRecipeData_RejectsBadPlugin(t *testing.T) {
	cases := []string{
		"with space",
		"-leading",
		"@nomain",
		"name@",
	}
	for _, p := range cases {
		body := `{"marketplace_source":"a/b","plugin":` + jsonString(p) + `}`
		_, err := recipes.ValidateRecipeData(store.RecipeTypeClaudePlugin, json.RawMessage(body))
		if !errors.Is(err, recipes.ErrInvalidRecipe) {
			t.Errorf("plugin %q should be rejected, got err=%v", p, err)
		}
	}
}

func TestValidateRecipeData_SparsePathsValidated(t *testing.T) {
	cases := []string{
		`{"marketplace_source":"a/b","plugin":"x","sparse_paths":["plugins/foo","plugins/bar"]}`, // ok
		`{"marketplace_source":"a/b","plugin":"x","sparse_paths":[""]}`,                          // empty
		`{"marketplace_source":"a/b","plugin":"x","sparse_paths":["../escape"]}`,                 // traversal
		`{"marketplace_source":"a/b","plugin":"x","sparse_paths":["/abs"]}`,                      // absolute
	}
	if _, err := recipes.ValidateRecipeData(store.RecipeTypeClaudePlugin, json.RawMessage(cases[0])); err != nil {
		t.Errorf("happy sparse_paths rejected: %v", err)
	}
	for _, c := range cases[1:] {
		if _, err := recipes.ValidateRecipeData(store.RecipeTypeClaudePlugin, json.RawMessage(c)); !errors.Is(err, recipes.ErrInvalidRecipe) {
			t.Errorf("expected rejection for %q, got %v", c, err)
		}
	}
}

func TestServiceCreate_HappyPath(t *testing.T) {
	svc, st := newService(t)
	r, err := svc.Create(&recipes.CreateInput{
		Slug:        "claude-mem",
		DisplayName: "Claude Mem",
		Description: "memory plugin",
		RecipeType:  store.RecipeTypeClaudePlugin,
		RecipeData:  json.RawMessage(`{"marketplace_source":"thedotmack/claude-mem","plugin":"claude-mem@thedotmack"}`),
		Visibility:  "public",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.ID == "" || r.Slug != "claude-mem" {
		t.Errorf("Create returned %+v", r)
	}
	got, _ := st.GetRecipeBySlug("claude-mem")
	if got == nil {
		t.Fatal("recipe not persisted")
	}
}

func TestServiceCreate_DefaultsDisplayNameToSlug(t *testing.T) {
	svc, _ := newService(t)
	r, err := svc.Create(&recipes.CreateInput{
		Slug:       "auto-name",
		RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.DisplayName != "auto-name" {
		t.Errorf("DisplayName = %q, want fallback to slug", r.DisplayName)
	}
}

func TestServiceCreate_RejectsInvalidRecipe(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.Create(&recipes.CreateInput{
		Slug:       "bad",
		RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{}`),
	})
	if !errors.Is(err, recipes.ErrInvalidRecipe) {
		t.Fatalf("expected ErrInvalidRecipe, got %v", err)
	}
}

func TestServiceUpdate_PreservesShapeOnPartialPatch(t *testing.T) {
	svc, _ := newService(t)
	r, err := svc.Create(&recipes.CreateInput{
		Slug:       "patch-me",
		RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`),
		Visibility: "public",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	updated, err := svc.Update(r.ID, &recipes.UpdateInput{
		DisplayName: "Renamed",
		Description: "new desc",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.DisplayName != "Renamed" {
		t.Errorf("DisplayName = %q", updated.DisplayName)
	}
	// recipe_data should be preserved (not zeroed)
	var got recipes.ClaudePluginRecipe
	if err := json.Unmarshal(updated.RecipeData, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.MarketplaceSource != "a/b" || got.Plugin != "x" {
		t.Errorf("recipe_data lost: %+v", got)
	}
}

func TestServiceUpdate_ValidatesNewRecipeData(t *testing.T) {
	svc, _ := newService(t)
	r, err := svc.Create(&recipes.CreateInput{
		Slug:       "validation-target",
		RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = svc.Update(r.ID, &recipes.UpdateInput{
		RecipeData: json.RawMessage(`{"marketplace_source":"","plugin":""}`),
	})
	if !errors.Is(err, recipes.ErrInvalidRecipe) {
		t.Errorf("expected ErrInvalidRecipe on bad update, got %v", err)
	}
}

func TestServiceResolveBySlug_HidesYanked(t *testing.T) {
	svc, st := newService(t)
	r, _ := svc.Create(&recipes.CreateInput{
		Slug:       "hideable",
		RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`),
	})
	if err := st.YankRecipe(r.ID); err != nil {
		t.Fatalf("YankRecipe: %v", err)
	}
	if _, err := svc.ResolveBySlug("hideable"); !errors.Is(err, recipes.ErrRecipeNotFound) {
		t.Fatalf("expected ErrRecipeNotFound for yanked, got %v", err)
	}
}

// jsonString is a tiny helper to embed a string literal as a JSON value
// without taking on a quoting library dependency.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
