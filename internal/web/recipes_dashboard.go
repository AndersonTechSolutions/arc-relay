// Package web — setup-recipe registry dashboard handlers.
//
// All handlers in this file require session-cookie auth (h.requireAuth wraps
// them at registration time). Read endpoints are scoped per visibility ACL;
// write endpoints (create, yank, delete) require admin role and verify the
// CSRF token from the form post.
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/comma-compliance/arc-relay/internal/recipes"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// HandleRecipesList renders /recipes — the landing page listing recipes the
// user can see. Admins see all (incl. yanked); non-admins see public + their
// explicit assignments (yanked filtered).
func (h *Handlers) HandleRecipesList(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/recipes" {
		http.NotFound(w, r)
		return
	}
	user := getUser(r)

	var (
		list []*store.SetupRecipe
		err  error
	)
	if user.Role == "admin" {
		list, err = h.recipeStore.ListRecipes()
	} else {
		var assigned []*store.AssignedRecipe
		assigned, err = h.recipeStore.AssignedForUser(user.ID)
		if err == nil {
			list = make([]*store.SetupRecipe, 0, len(assigned))
			for _, a := range assigned {
				list = append(list, a.Recipe)
			}
		}
	}
	if err != nil {
		slog.Warn("recipes dashboard list", "user", user.ID, "err", err)
		http.Error(w, "failed to list recipes", http.StatusInternalServerError)
		return
	}

	h.render(w, r, "recipes.html", map[string]any{
		"Nav":     "recipes",
		"User":    user,
		"Recipes": list,
	})
}

// HandleRecipeNew handles GET/POST /recipes/new. Admin-only.
func (h *Handlers) HandleRecipeNew(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.URL.Path != "/recipes/new" {
		http.NotFound(w, r)
		return
	}
	user := getUser(r)

	if r.Method == http.MethodGet {
		h.renderRecipeNewForm(w, r, user, "", nil)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderRecipeNewForm(w, r, user, "Invalid form: "+err.Error(), nil)
		return
	}
	if !h.validateCSRF(r, getSessionID(r.Context())) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	form := recipeFormValues(r)
	data, err := buildClaudePluginRecipeData(form)
	if err != nil {
		h.renderRecipeNewForm(w, r, user, err.Error(), form)
		return
	}

	res, err := h.recipeSvc.Create(&recipes.CreateInput{
		Slug:        form["slug"],
		DisplayName: form["display_name"],
		Description: form["description"],
		RecipeType:  store.RecipeTypeClaudePlugin,
		RecipeData:  data,
		Visibility:  form["visibility"],
		CreatedBy:   user.ID,
	})
	if err != nil {
		switch {
		case errors.Is(err, recipes.ErrInvalidRecipe):
			h.renderRecipeNewForm(w, r, user, err.Error(), form)
		case errors.Is(err, store.ErrRecipeSlugConflict):
			h.renderRecipeNewForm(w, r, user, fmt.Sprintf("Slug %q is already in use.", form["slug"]), form)
		default:
			slog.Warn("recipes dashboard create", "slug", form["slug"], "err", err)
			h.renderRecipeNewForm(w, r, user, "Internal error while creating recipe.", form)
		}
		return
	}
	http.Redirect(w, r, "/recipes/"+res.Slug, http.StatusFound)
}

// HandleRecipeRoutes routes /recipes/{slug}[/action]. GET on a bare slug
// renders the detail page; POSTs implement yank/unyank/delete.
func (h *Handlers) HandleRecipeRoutes(w http.ResponseWriter, r *http.Request) {
	user := getUser(r)
	rest := strings.TrimPrefix(r.URL.Path, "/recipes/")
	if rest == "" {
		http.Redirect(w, r, "/recipes", http.StatusFound)
		return
	}
	parts := strings.Split(rest, "/")
	slug := parts[0]

	recipe, err := h.recipeStore.GetRecipeBySlug(slug)
	if err != nil {
		slog.Warn("recipe dashboard lookup", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if recipe == nil {
		http.NotFound(w, r)
		return
	}
	if user.Role != "admin" && !h.userCanReadRecipe(user, recipe) {
		http.NotFound(w, r)
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			h.renderRecipeDetail(w, r, user, recipe)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// Mutating actions: admin-only, CSRF-checked, POST-only.
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if !h.validateCSRF(r, getSessionID(r.Context())) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	switch parts[1] {
	case "yank":
		if err := h.recipeStore.YankRecipe(recipe.ID); err != nil {
			slog.Warn("yank recipe", "slug", slug, "err", err)
			http.Error(w, "yank failed", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/recipes/"+slug, http.StatusFound)
	case "unyank":
		if err := h.recipeStore.UnyankRecipe(recipe.ID); err != nil {
			slog.Warn("unyank recipe", "slug", slug, "err", err)
			http.Error(w, "unyank failed", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/recipes/"+slug, http.StatusFound)
	case "delete":
		if err := h.recipeStore.DeleteRecipe(recipe.ID); err != nil {
			slog.Warn("delete recipe", "slug", slug, "err", err)
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/recipes", http.StatusFound)
	default:
		http.NotFound(w, r)
	}
}

// renderRecipeDetail builds the data context for /recipes/{slug}.
func (h *Handlers) renderRecipeDetail(w http.ResponseWriter, r *http.Request, user *store.User, recipe *store.SetupRecipe) {
	var assignments []*store.SetupRecipeAssignment
	var err error
	if user.Role == "admin" {
		assignments, err = h.recipeStore.ListAssignmentsForRecipe(recipe.ID)
		if err != nil {
			slog.Warn("recipe detail assignments", "slug", recipe.Slug, "err", err)
			http.Error(w, "failed to load assignments", http.StatusInternalServerError)
			return
		}
	}
	// Pretty-print recipe_data for the template.
	prettyData := recipe.RecipeData
	if buf, err := json.MarshalIndent(json.RawMessage(recipe.RecipeData), "", "  "); err == nil {
		prettyData = buf
	}

	h.render(w, r, "recipe_detail.html", map[string]any{
		"Nav":         "recipes",
		"User":        user,
		"Recipe":      recipe,
		"PrettyData":  string(prettyData),
		"Assignments": assignments,
	})
}

// renderRecipeNewForm renders /recipes/new with the given error + prefilled
// form values (on validation failure the user shouldn't have to re-type).
func (h *Handlers) renderRecipeNewForm(w http.ResponseWriter, r *http.Request, user *store.User, errMsg string, form map[string]string) {
	if form == nil {
		form = map[string]string{}
	}
	h.render(w, r, "recipe_new.html", map[string]any{
		"Nav":   "recipes",
		"User":  user,
		"Error": errMsg,
		"Form":  form,
	})
}

// userCanReadRecipe mirrors AssignedForUser's WHERE clause: yanked hidden,
// public visible, restricted only with assignment.
func (h *Handlers) userCanReadRecipe(user *store.User, recipe *store.SetupRecipe) bool {
	if recipe.YankedAt != nil {
		return false
	}
	if recipe.Visibility == "public" {
		return true
	}
	assigns, err := h.recipeStore.ListAssignmentsForRecipe(recipe.ID)
	if err != nil {
		return false
	}
	for _, a := range assigns {
		if a.UserID == user.ID {
			return true
		}
	}
	return false
}

// recipeFormValues collects relevant form fields into a map for prefill +
// recipe_data construction. We don't require all fields — the service layer
// surfaces missing-field errors with specific messages.
func recipeFormValues(r *http.Request) map[string]string {
	return map[string]string{
		"slug":               strings.TrimSpace(r.FormValue("slug")),
		"display_name":       strings.TrimSpace(r.FormValue("display_name")),
		"description":        strings.TrimSpace(r.FormValue("description")),
		"visibility":         r.FormValue("visibility"),
		"marketplace_source": strings.TrimSpace(r.FormValue("marketplace_source")),
		"plugin":             strings.TrimSpace(r.FormValue("plugin")),
		"enabled":            r.FormValue("enabled"),
		"sparse_paths":       strings.TrimSpace(r.FormValue("sparse_paths")),
	}
}

// buildClaudePluginRecipeData constructs the recipe_data JSON for a
// claude_plugin recipe from form values. The returned bytes still go through
// the service-layer validator so per-field error messages stay consistent
// across CLI and web paths.
func buildClaudePluginRecipeData(form map[string]string) (json.RawMessage, error) {
	enabled := true
	switch strings.ToLower(form["enabled"]) {
	case "", "on", "true", "1", "yes":
		enabled = true
	case "off", "false", "0", "no":
		enabled = false
	}

	rec := recipes.ClaudePluginRecipe{
		MarketplaceSource: form["marketplace_source"],
		Plugin:            form["plugin"],
		Enabled:           enabled,
	}
	if raw := form["sparse_paths"]; raw != "" {
		for _, p := range strings.Split(raw, ",") {
			if t := strings.TrimSpace(p); t != "" {
				rec.SparsePaths = append(rec.SparsePaths, t)
			}
		}
	}
	return json.Marshal(&rec)
}
