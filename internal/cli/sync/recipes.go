// Package sync — setup-recipe install + sync.
//
// Recipes describe HOW to install third-party Claude Code plugins. The
// RecipeManager runs each recipe via the supported `claude plugin` CLI
// (marketplace add → install → optional disable). Sync is install-only:
// every assigned recipe is installed (idempotent — `claude plugin install`
// no-ops on already-installed plugins). We deliberately do NOT auto-uninstall
// when a recipe is unassigned because `claude plugin list` does not
// distinguish recipe-installed from hand-installed plugins, and silently
// nuking a hand-installed plugin would surprise the user.
package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/comma-compliance/arc-relay/internal/cli/relay"
)

// ClaudeRunner abstracts the `claude` CLI so tests can swap in a recorder.
// Production wires through ExecClaudeRunner which shells out for real.
type ClaudeRunner interface {
	Run(ctx context.Context, args ...string) (stdout []byte, err error)
}

// ExecClaudeRunner is the production runner — invokes the `claude` binary
// found on PATH. Stdin is closed so Claude Code's plugin commands cannot
// hang waiting for input even if a future version adds prompts.
type ExecClaudeRunner struct {
	// Binary overrides the binary path; default "claude".
	Binary string
	// Timeout caps each invocation; default 5 minutes (plugin installs can
	// run npm/bun/git steps).
	Timeout time.Duration
}

// Run shells out to the configured Claude binary. Returns stdout on success;
// on failure, the error message includes both stderr and stdout so the
// caller can render whatever Claude printed.
func (e *ExecClaudeRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	bin := e.Binary
	if bin == "" {
		bin = "claude"
	}
	timeout := e.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	// #nosec G204 -- bin is the literal "claude" unless overridden in Go code;
	// exec.Command does not use a shell, so recipe-supplied values become
	// single argv elements and cannot inject commands.
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("`%s %s` failed: %w; stderr: %s; stdout: %s",
			bin, strings.Join(args, " "), err,
			strings.TrimSpace(stderr.String()),
			strings.TrimSpace(stdout.String()))
	}
	return stdout.Bytes(), nil
}

// RecipeSyncReport summarizes a sync run.
type RecipeSyncReport struct {
	Installed []RecipeSyncAction `json:"installed"`
	Unchanged []RecipeSyncAction `json:"unchanged"`
	Errors    []RecipeSyncError  `json:"errors,omitempty"`
}

// RecipeSyncAction is one recipe's result.
type RecipeSyncAction struct {
	Slug        string `json:"slug"`
	Plugin      string `json:"plugin"`
	Marketplace string `json:"marketplace"`
}

// RecipeSyncError captures a per-recipe failure without aborting the run.
type RecipeSyncError struct {
	Slug    string `json:"slug"`
	Message string `json:"message"`
}

// RecipeSyncOptions controls a sync run.
type RecipeSyncOptions struct {
	DryRun bool
}

// RecipeManager bundles everything the recipe subcommands need: relay HTTP
// client + the claude binary runner.
type RecipeManager struct {
	Client *relay.Client
	Runner ClaudeRunner
}

// pluginListEntry mirrors a single row of `claude plugin list --json`. We
// only consume the fields we care about; everything else is tolerated.
type pluginListEntry struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// ListInstalledPlugins shells `claude plugin list --json` and returns a
// slug-keyed map. Returns an empty map if `claude` is missing or the call
// fails — most callers want best-effort introspection that doesn't break the
// flow when claude isn't on PATH.
func (m *RecipeManager) ListInstalledPlugins(ctx context.Context) (map[string]pluginListEntry, error) {
	out, err := m.Runner.Run(ctx, "plugin", "list", "--json")
	if err != nil {
		return nil, err
	}
	var entries []pluginListEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("parse plugin list: %w", err)
	}
	indexed := make(map[string]pluginListEntry, len(entries))
	for _, e := range entries {
		indexed[e.ID] = e
	}
	return indexed, nil
}

// Install runs one recipe end-to-end. For recipe_type=claude_plugin:
// 1. claude plugin marketplace add <marketplace_source>      (idempotent on the CLI)
// 2. claude plugin install <plugin>                          (idempotent)
// 3. claude plugin disable <plugin>                          (only if Enabled=false AND it's currently enabled)
//
// The marketplace-add step is run unconditionally — `claude plugin
// marketplace add` is a no-op when the marketplace already exists, so this
// keeps the call sequence simple.
func (m *RecipeManager) Install(ctx context.Context, r *relay.Recipe) error {
	if r.RecipeType != "claude_plugin" {
		return fmt.Errorf("unsupported recipe_type %q", r.RecipeType)
	}
	var data relay.ClaudePluginRecipeData
	if err := json.Unmarshal(r.RecipeData, &data); err != nil {
		return fmt.Errorf("parse recipe_data: %w", err)
	}
	if data.MarketplaceSource == "" || data.Plugin == "" {
		return errors.New("recipe_data missing marketplace_source or plugin")
	}

	args := []string{"plugin", "marketplace", "add", data.MarketplaceSource, "--scope", "user"}
	for _, p := range data.SparsePaths {
		args = append(args, "--sparse", p)
	}
	if _, err := m.Runner.Run(ctx, args...); err != nil {
		return fmt.Errorf("marketplace add: %w", err)
	}

	if _, err := m.Runner.Run(ctx, "plugin", "install", data.Plugin, "--scope", "user"); err != nil {
		return fmt.Errorf("plugin install: %w", err)
	}

	if !data.Enabled {
		// Only disable if currently enabled — `claude plugin disable` errors
		// when the plugin is already disabled in some versions.
		installed, err := m.ListInstalledPlugins(ctx)
		if err == nil {
			if entry, ok := installed[data.Plugin]; ok && entry.Enabled {
				if _, err := m.Runner.Run(ctx, "plugin", "disable", data.Plugin); err != nil {
					return fmt.Errorf("plugin disable: %w", err)
				}
			}
		}
	}
	return nil
}

// Uninstall is the explicit teardown path. `arc-sync recipe sync` never calls
// this — the user invokes it directly when they want a plugin gone.
func (m *RecipeManager) Uninstall(ctx context.Context, r *relay.Recipe) error {
	var data relay.ClaudePluginRecipeData
	if err := json.Unmarshal(r.RecipeData, &data); err != nil {
		return fmt.Errorf("parse recipe_data: %w", err)
	}
	if data.Plugin == "" {
		return errors.New("recipe_data missing plugin")
	}
	if _, err := m.Runner.Run(ctx, "plugin", "uninstall", data.Plugin); err != nil {
		return fmt.Errorf("plugin uninstall: %w", err)
	}
	return nil
}

// Sync installs every recipe in the relay's /assigned response. Idempotent:
// already-installed plugins are surfaced as Unchanged; install errors are
// recorded per-recipe so a single bad recipe doesn't abort the whole run.
//
// Sync is install-only — it never uninstalls plugins, even if the relay no
// longer assigns a recipe. See the package doc for the rationale.
func (m *RecipeManager) Sync(ctx context.Context, opts RecipeSyncOptions) (*RecipeSyncReport, error) {
	assigned, err := m.Client.ListAssignedRecipes()
	if err != nil {
		return nil, fmt.Errorf("list assigned: %w", err)
	}

	// Best-effort snapshot of currently-installed plugins so we can surface
	// already-installed entries as Unchanged without re-running install.
	// Errors here are non-fatal — we'll just install everything (idempotent).
	installed, _ := m.ListInstalledPlugins(ctx)

	report := &RecipeSyncReport{}
	for _, a := range assigned {
		if a.Recipe == nil {
			continue
		}
		var data relay.ClaudePluginRecipeData
		if err := json.Unmarshal(a.Recipe.RecipeData, &data); err != nil {
			report.Errors = append(report.Errors, RecipeSyncError{
				Slug: a.Recipe.Slug, Message: "parse recipe_data: " + err.Error(),
			})
			continue
		}
		if installed != nil {
			if _, ok := installed[data.Plugin]; ok {
				report.Unchanged = append(report.Unchanged, RecipeSyncAction{
					Slug: a.Recipe.Slug, Plugin: data.Plugin, Marketplace: data.MarketplaceSource,
				})
				continue
			}
		}
		if opts.DryRun {
			report.Installed = append(report.Installed, RecipeSyncAction{
				Slug: a.Recipe.Slug, Plugin: data.Plugin, Marketplace: data.MarketplaceSource,
			})
			continue
		}
		if err := m.Install(ctx, a.Recipe); err != nil {
			report.Errors = append(report.Errors, RecipeSyncError{
				Slug: a.Recipe.Slug, Message: err.Error(),
			})
			continue
		}
		report.Installed = append(report.Installed, RecipeSyncAction{
			Slug: a.Recipe.Slug, Plugin: data.Plugin, Marketplace: data.MarketplaceSource,
		})
	}
	return report, nil
}
