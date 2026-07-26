// Package recipes implements the setup-recipe registry service: per-recipe-type
// shape validation, CRUD orchestration on top of store.SetupRecipeStore, and
// future hooks for client-side install reporting.
//
// IMPORTANT: this package never executes a recipe. Install actually runs on
// the arc-sync client (e.g. via `claude plugin install`); the relay only
// stores recipe metadata. Keeping execution off the relay means a compromised
// relay can publish bad recipes but cannot directly run code on every client
// — sync is opt-in.
package recipes

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/comma-compliance/arc-relay/internal/store"
)

// ErrInvalidRecipe is the umbrella error returned for any shape-validation
// failure. Callers can use errors.Is to render as 400.
var ErrInvalidRecipe = errors.New("invalid recipe")

// ErrRecipeNotFound is returned when a slug or id lookup fails.
var ErrRecipeNotFound = errors.New("recipe not found")

// ClaudePluginRecipe is the recipe_data shape for recipe_type = "claude_plugin".
//
// The execution model on the client is roughly:
//
//	claude plugin marketplace add <MarketplaceSource>
//	claude plugin install <Plugin>      // typically "name@marketplace"
//	if !Enabled { claude plugin disable <Plugin> }
type ClaudePluginRecipe struct {
	// MarketplaceSource is the argument to `claude plugin marketplace add`.
	// Accepts a GitHub "owner/repo" reference, a git URL, or a local path.
	MarketplaceSource string `json:"marketplace_source"`
	// Plugin is the argument to `claude plugin install`. Typical form is
	// "<plugin-name>@<marketplace-name>"; the client passes it through.
	Plugin string `json:"plugin"`
	// Enabled controls whether the plugin should be enabled after install.
	// Defaults to true (most users want their installed plugins active).
	Enabled bool `json:"enabled"`
	// SparsePaths optionally restricts the marketplace clone to specific
	// directories via git sparse-checkout. Useful for monorepos. Empty = full clone.
	SparsePaths []string `json:"sparse_paths,omitempty"`
}

// pluginIdentRe matches the relaxed `name@marketplace` form arc-sync clients
// pass to `claude plugin install`. We don't require the @marketplace half
// because `claude plugin install <name>` is also valid (lets the CLI resolve
// across known marketplaces).
var pluginIdentRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*(@[a-zA-Z0-9][a-zA-Z0-9_.-]*)?$`)

// Service wraps a SetupRecipeStore and adds shape validation per recipe_type.
type Service struct {
	store *store.SetupRecipeStore
}

// New constructs a Service.
func New(s *store.SetupRecipeStore) *Service {
	return &Service{store: s}
}

// CreateInput is the validated payload for Service.Create.
type CreateInput struct {
	Slug        string
	DisplayName string
	Description string
	RecipeType  string
	RecipeData  json.RawMessage
	Visibility  string
	CreatedBy   string // user id; empty -> stored as NULL
}

// Create validates the recipe shape (per recipe_type) and persists it.
// Returns ErrInvalidRecipe wrapping a diagnostic on validation failure
// (including slug regex violations and CHECK-constraint failures).
func (s *Service) Create(in *CreateInput) (*store.SetupRecipe, error) {
	if err := store.ValidateSlug(in.Slug); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
	}
	normalized, err := ValidateRecipeData(in.RecipeType, in.RecipeData)
	if err != nil {
		return nil, err
	}
	display := strings.TrimSpace(in.DisplayName)
	if display == "" {
		display = in.Slug
	}
	r := &store.SetupRecipe{
		Slug:        in.Slug,
		DisplayName: display,
		Description: strings.TrimSpace(in.Description),
		RecipeType:  in.RecipeType,
		RecipeData:  normalized,
		Visibility:  in.Visibility,
		CreatedBy:   nullable(in.CreatedBy),
	}
	if err := s.store.CreateRecipe(r); err != nil {
		// Slug-already-exists is its own sentinel; preserve so handlers
		// can render 409. Everything else here is treated as invalid input.
		if errors.Is(err, store.ErrRecipeSlugConflict) {
			return nil, err
		}
		msg := err.Error()
		if strings.Contains(msg, "CHECK constraint failed") ||
			strings.Contains(msg, "invalid visibility") ||
			strings.Contains(msg, "recipe_type is required") {
			return nil, fmt.Errorf("%w: %v", ErrInvalidRecipe, err)
		}
		return nil, err
	}
	return r, nil
}

// UpdateInput is the patch shape for Service.Update.
type UpdateInput struct {
	DisplayName string
	Description string
	Visibility  string          // empty = preserve
	RecipeData  json.RawMessage // empty = preserve current
}

// Update applies a patch to an existing recipe. RecipeType is immutable —
// callers wanting to change the type should delete and recreate.
func (s *Service) Update(id string, in *UpdateInput) (*store.SetupRecipe, error) {
	current, err := s.store.GetRecipe(id)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, ErrRecipeNotFound
	}
	data := in.RecipeData
	if len(data) > 0 {
		normalized, err := ValidateRecipeData(current.RecipeType, data)
		if err != nil {
			return nil, err
		}
		data = normalized
	} else {
		data = current.RecipeData
	}
	if err := s.store.UpdateRecipe(id, in.DisplayName, in.Description, in.Visibility, data); err != nil {
		return nil, err
	}
	return s.store.GetRecipe(id)
}

// ResolveBySlug returns a recipe by slug, or ErrRecipeNotFound if missing
// or yanked.
func (s *Service) ResolveBySlug(slug string) (*store.SetupRecipe, error) {
	r, err := s.store.GetRecipeBySlug(slug)
	if err != nil {
		return nil, err
	}
	if r == nil || r.YankedAt != nil {
		return nil, ErrRecipeNotFound
	}
	return r, nil
}

// AssignedForUser is a thin pass-through to the store. Surfaces the same
// shape `arc-sync recipe sync` consumes.
func (s *Service) AssignedForUser(userID string) ([]*store.AssignedRecipe, error) {
	return s.store.AssignedForUser(userID)
}

// ValidateRecipeData inspects the JSON payload against the schema for the
// given recipe_type and returns a canonicalized form (re-marshaled with the
// declared field set, defaults applied). Returns ErrInvalidRecipe on any
// type-level violation.
func ValidateRecipeData(recipeType string, data json.RawMessage) (json.RawMessage, error) {
	if recipeType != store.RecipeTypeClaudePlugin {
		return nil, fmt.Errorf("%w: unsupported recipe_type %q (only %q is supported)",
			ErrInvalidRecipe, recipeType, store.RecipeTypeClaudePlugin)
	}
	if len(data) == 0 || string(data) == "{}" {
		return nil, fmt.Errorf("%w: recipe_data is required for %s", ErrInvalidRecipe, recipeType)
	}
	var r ClaudePluginRecipe
	r.Enabled = true // default before unmarshal so omitted enabled => true
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%w: parsing recipe_data: %v", ErrInvalidRecipe, err)
	}
	r.MarketplaceSource = strings.TrimSpace(r.MarketplaceSource)
	r.Plugin = strings.TrimSpace(r.Plugin)
	if r.MarketplaceSource == "" {
		return nil, fmt.Errorf("%w: marketplace_source is required", ErrInvalidRecipe)
	}
	if r.Plugin == "" {
		return nil, fmt.Errorf("%w: plugin is required", ErrInvalidRecipe)
	}
	if !validMarketplaceSource(r.MarketplaceSource) {
		return nil, fmt.Errorf("%w: marketplace_source %q is not a recognized form (expect github owner/repo, git URL, or local path)",
			ErrInvalidRecipe, r.MarketplaceSource)
	}
	if !pluginIdentRe.MatchString(r.Plugin) {
		return nil, fmt.Errorf("%w: plugin %q must match %s",
			ErrInvalidRecipe, r.Plugin, pluginIdentRe.String())
	}
	for i, p := range r.SparsePaths {
		clean := strings.TrimSpace(p)
		if clean == "" || strings.Contains(clean, "..") || strings.HasPrefix(clean, "/") {
			return nil, fmt.Errorf("%w: sparse_paths[%d]=%q must be a non-empty relative path",
				ErrInvalidRecipe, i, p)
		}
		r.SparsePaths[i] = clean
	}
	out, err := json.Marshal(&r)
	if err != nil {
		return nil, fmt.Errorf("re-marshaling recipe_data: %w", err)
	}
	return out, nil
}

// validMarketplaceSource accepts:
//   - GitHub shortform "owner/repo"
//   - http(s):// or git:// or git@... URLs that parse cleanly
//   - absolute local paths beginning with "/"
//   - explicit "file://" URIs
func validMarketplaceSource(src string) bool {
	if strings.HasPrefix(src, "/") {
		return true
	}
	if strings.HasPrefix(src, "file://") || strings.HasPrefix(src, "http://") ||
		strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "git://") ||
		strings.HasPrefix(src, "ssh://") {
		_, err := url.Parse(src)
		return err == nil
	}
	if strings.HasPrefix(src, "git@") {
		// scp-style git URLs (git@github.com:owner/repo.git). Don't try to
		// fully parse — just sanity check that the colon and slash are present.
		return strings.Contains(src, ":") && strings.Contains(src, "/")
	}
	// GitHub owner/repo: alphanumeric + dash + underscore + dot,
	// exactly one slash, no leading/trailing slash, no whitespace.
	if strings.Count(src, "/") != 1 || strings.ContainsAny(src, " \t\n") {
		return false
	}
	parts := strings.Split(src, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	return githubIdentRe.MatchString(parts[0]) && githubIdentRe.MatchString(parts[1])
}

var githubIdentRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// nullable returns nil for the empty string and a pointer otherwise.
// Lets callers leave the audit field blank without writing a zero-length string.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
