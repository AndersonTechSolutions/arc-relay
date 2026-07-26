package sync_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/cli/relay"
	"github.com/comma-compliance/arc-relay/internal/cli/sync"
)

// fakeRunner is a stand-in for the real `claude` CLI. Each Run() call is
// recorded so tests can assert on the exact command sequence the manager
// produced. Tests can also pre-stage canned outputs and errors per command.
type fakeRunner struct {
	calls    [][]string
	listJSON string           // canned response for `plugin list --json`
	errs     map[string]error // command-prefix → error to return
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{errs: map[string]error{}}
}

func (f *fakeRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{}, args...))
	joined := strings.Join(args, " ")
	for prefix, err := range f.errs {
		if strings.HasPrefix(joined, prefix) {
			return nil, err
		}
	}
	if len(args) >= 3 && args[0] == "plugin" && args[1] == "list" && args[2] == "--json" {
		if f.listJSON == "" {
			return []byte("[]"), nil
		}
		return []byte(f.listJSON), nil
	}
	return []byte("OK"), nil
}

// callsContaining returns true if any recorded Run() invocation matches the
// given prefix (joined as space-separated args).
func (f *fakeRunner) calledWith(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return true
		}
	}
	return false
}

// fakeRecipeRelay is a tiny httptest server that mimics /api/recipes/* for
// install/sync flows.
type fakeRecipeRelay struct {
	*httptest.Server
	assigned []*relay.AssignedRecipe
	recipes  map[string]*relay.Recipe
}

func newFakeRecipeRelay(t *testing.T) *fakeRecipeRelay {
	t.Helper()
	fr := &fakeRecipeRelay{recipes: map[string]*relay.Recipe{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/recipes/assigned", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"assigned": fr.assigned})
	})
	mux.HandleFunc("/api/recipes/", func(w http.ResponseWriter, r *http.Request) {
		slug := strings.TrimPrefix(r.URL.Path, "/api/recipes/")
		if rec, ok := fr.recipes[slug]; ok {
			_ = json.NewEncoder(w).Encode(rec)
			return
		}
		http.NotFound(w, r)
	})
	fr.Server = httptest.NewServer(mux)
	t.Cleanup(fr.Close)
	return fr
}

func (fr *fakeRecipeRelay) addRecipe(slug, plugin, market string, enabled bool) *relay.Recipe {
	data, _ := json.Marshal(relay.ClaudePluginRecipeData{
		MarketplaceSource: market,
		Plugin:            plugin,
		Enabled:           enabled,
	})
	rec := &relay.Recipe{
		Slug: slug, RecipeType: "claude_plugin", Visibility: "public", RecipeData: data,
	}
	fr.recipes[slug] = rec
	return rec
}

func newRecipeManager(t *testing.T, fr *fakeRecipeRelay, runner sync.ClaudeRunner) *sync.RecipeManager {
	t.Helper()
	return &sync.RecipeManager{
		Client: &relay.Client{
			BaseURL:    fr.URL,
			APIKey:     "test-key",
			HTTPClient: fr.Client(),
		},
		Runner: runner,
	}
}

func TestRecipeInstall_RunsExpectedCommands(t *testing.T) {
	fr := newFakeRecipeRelay(t)
	rec := fr.addRecipe("claude-mem", "claude-mem@thedotmack", "thedotmack/claude-mem", true)
	runner := newFakeRunner()
	mgr := newRecipeManager(t, fr, runner)

	if err := mgr.Install(context.Background(), rec); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !runner.calledWith("plugin marketplace add thedotmack/claude-mem") {
		t.Errorf("expected marketplace add, got %v", runner.calls)
	}
	if !runner.calledWith("plugin install claude-mem@thedotmack") {
		t.Errorf("expected plugin install, got %v", runner.calls)
	}
	// enabled=true: should NOT call disable
	if runner.calledWith("plugin disable") {
		t.Errorf("disable was called when enabled=true: %v", runner.calls)
	}
}

func TestRecipeInstall_WithSparsePaths(t *testing.T) {
	fr := newFakeRecipeRelay(t)
	data, _ := json.Marshal(relay.ClaudePluginRecipeData{
		MarketplaceSource: "anthropics/claude-plugins-official",
		Plugin:            "foo@official",
		Enabled:           true,
		SparsePaths:       []string{".claude-plugin", "plugins/foo"},
	})
	rec := &relay.Recipe{Slug: "foo", RecipeType: "claude_plugin", RecipeData: data}
	runner := newFakeRunner()
	mgr := newRecipeManager(t, fr, runner)

	if err := mgr.Install(context.Background(), rec); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !runner.calledWith("plugin marketplace add anthropics/claude-plugins-official --scope user --sparse .claude-plugin --sparse plugins/foo") {
		t.Errorf("sparse flags missing: %v", runner.calls[0])
	}
}

func TestRecipeInstall_DisablesWhenRecipeSaysSo(t *testing.T) {
	fr := newFakeRecipeRelay(t)
	rec := fr.addRecipe("dis", "dis-plugin@market", "market/dis", false)
	runner := newFakeRunner()
	// Pretend it just got installed and is enabled — Install should disable it.
	runner.listJSON = `[{"id":"dis-plugin@market","enabled":true}]`
	mgr := newRecipeManager(t, fr, runner)

	if err := mgr.Install(context.Background(), rec); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !runner.calledWith("plugin disable dis-plugin@market") {
		t.Errorf("expected plugin disable, got %v", runner.calls)
	}
}

func TestRecipeInstall_SkipsDisableWhenAlreadyDisabled(t *testing.T) {
	fr := newFakeRecipeRelay(t)
	rec := fr.addRecipe("dis", "dis-plugin@market", "market/dis", false)
	runner := newFakeRunner()
	runner.listJSON = `[{"id":"dis-plugin@market","enabled":false}]`
	mgr := newRecipeManager(t, fr, runner)

	if err := mgr.Install(context.Background(), rec); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if runner.calledWith("plugin disable") {
		t.Errorf("should not call disable on already-disabled plugin: %v", runner.calls)
	}
}

func TestRecipeInstall_FailsOnUnknownRecipeType(t *testing.T) {
	fr := newFakeRecipeRelay(t)
	rec := &relay.Recipe{Slug: "weird", RecipeType: "shell_script",
		RecipeData: json.RawMessage(`{"script":"echo hi"}`)}
	runner := newFakeRunner()
	mgr := newRecipeManager(t, fr, runner)
	if err := mgr.Install(context.Background(), rec); err == nil {
		t.Fatal("expected unsupported recipe_type error")
	}
}

func TestRecipeInstall_PropagatesCommandErrors(t *testing.T) {
	fr := newFakeRecipeRelay(t)
	rec := fr.addRecipe("fail", "fail@market", "market/fail", true)
	runner := newFakeRunner()
	runner.errs["plugin install"] = errors.New("simulated install failure")
	mgr := newRecipeManager(t, fr, runner)
	err := mgr.Install(context.Background(), rec)
	if err == nil {
		t.Fatal("expected install error to propagate")
	}
	if !strings.Contains(err.Error(), "simulated install failure") {
		t.Errorf("error message = %v", err)
	}
}

func TestRecipeSync_InstallsAssignedAndReportsUnchanged(t *testing.T) {
	fr := newFakeRecipeRelay(t)
	r1 := fr.addRecipe("a", "a-plugin@market-a", "market/a", true)
	r2 := fr.addRecipe("b", "b-plugin@market-b", "market/b", true)
	fr.assigned = []*relay.AssignedRecipe{{Recipe: r1}, {Recipe: r2}}

	runner := newFakeRunner()
	// b is already installed; a is fresh
	runner.listJSON = `[{"id":"b-plugin@market-b","enabled":true}]`
	mgr := newRecipeManager(t, fr, runner)

	report, err := mgr.Sync(context.Background(), sync.RecipeSyncOptions{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(report.Installed) != 1 || report.Installed[0].Slug != "a" {
		t.Errorf("Installed = %+v, want only a", report.Installed)
	}
	if len(report.Unchanged) != 1 || report.Unchanged[0].Slug != "b" {
		t.Errorf("Unchanged = %+v, want only b", report.Unchanged)
	}
	if len(report.Errors) != 0 {
		t.Errorf("Errors = %+v", report.Errors)
	}
}

func TestRecipeSync_DryRunDoesNotInstall(t *testing.T) {
	fr := newFakeRecipeRelay(t)
	r1 := fr.addRecipe("a", "a-plugin@market-a", "market/a", true)
	fr.assigned = []*relay.AssignedRecipe{{Recipe: r1}}
	runner := newFakeRunner()
	mgr := newRecipeManager(t, fr, runner)

	report, err := mgr.Sync(context.Background(), sync.RecipeSyncOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(report.Installed) != 1 {
		t.Errorf("dry-run should report Installed=1, got %+v", report)
	}
	if runner.calledWith("plugin marketplace add") {
		t.Error("dry-run should not invoke claude plugin commands")
	}
}

func TestRecipeSync_RecordsPerRecipeErrors(t *testing.T) {
	fr := newFakeRecipeRelay(t)
	good := fr.addRecipe("good", "good-plugin@market", "market/good", true)
	bad := fr.addRecipe("bad", "bad-plugin@market", "market/bad", true)
	fr.assigned = []*relay.AssignedRecipe{{Recipe: good}, {Recipe: bad}}

	runner := newFakeRunner()
	// fail only the bad install
	runner.errs["plugin install bad-plugin@market"] = errors.New("bad")
	mgr := newRecipeManager(t, fr, runner)

	report, err := mgr.Sync(context.Background(), sync.RecipeSyncOptions{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(report.Installed) != 1 || report.Installed[0].Slug != "good" {
		t.Errorf("Installed = %+v", report.Installed)
	}
	if len(report.Errors) != 1 || report.Errors[0].Slug != "bad" {
		t.Errorf("Errors = %+v", report.Errors)
	}
}

func TestRecipeUninstall_RunsPluginUninstall(t *testing.T) {
	fr := newFakeRecipeRelay(t)
	rec := fr.addRecipe("u", "u-plugin@market", "market/u", true)
	runner := newFakeRunner()
	mgr := newRecipeManager(t, fr, runner)
	if err := mgr.Uninstall(context.Background(), rec); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !runner.calledWith("plugin uninstall u-plugin@market") {
		t.Errorf("expected uninstall call, got %v", runner.calls)
	}
}
