package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/comma-compliance/arc-relay/internal/recipes"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// recipesBodyLimit caps create/update payload sizes. Recipes are pure JSON
// metadata (no archives), so the cap can be aggressive — anything above this
// is almost certainly malformed input.
const recipesBodyLimit = 64 * 1024 // 64 KiB

// RecipesHandlers wraps recipes.Service for HTTP. Like SkillsHandlers, it
// uses a closure to pull the authenticated user from context so the package
// stays free of an import-cycle dependency on internal/server. UserStore is
// used only to resolve username → user_id for the assignment endpoints.
type RecipesHandlers struct {
	svc           *recipes.Service
	store         *store.SetupRecipeStore
	users         *store.UserStore
	userFromCtx   func(context.Context) *store.User
	apiKeyFromCtx func(context.Context) *store.APIKey
}

// NewRecipesHandlers creates RecipesHandlers wired to the recipes service +
// stores. userStore is for assignment endpoints (username → user_id
// resolution). apiKeyFromCtx returns nil for session-cookie auth; capability
// gates fall back to user.Role == "admin" in that case.
func NewRecipesHandlers(
	svc *recipes.Service,
	st *store.SetupRecipeStore,
	users *store.UserStore,
	userFromCtx func(context.Context) *store.User,
	apiKeyFromCtx func(context.Context) *store.APIKey,
) *RecipesHandlers {
	return &RecipesHandlers{
		svc:           svc,
		store:         st,
		users:         users,
		userFromCtx:   userFromCtx,
		apiKeyFromCtx: apiKeyFromCtx,
	}
}

// HandleRecipes routes /api/recipes. GET = list-for-user, POST = create (admin).
func (h *RecipesHandlers) HandleRecipes(w http.ResponseWriter, r *http.Request) {
	user := h.userFromCtx(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.list(w, user)
	case http.MethodPost:
		// Create is gated by `recipes:write` so non-admin keys can publish.
		if !requireCapability(w, r, user, h.apiKeyFromCtx(r.Context()), "recipes:write") {
			return
		}
		h.create(w, r, user.ID)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// HandleAssigned returns the user's effective recipe set: public + restricted-
// with-explicit-grant. This is what `arc-sync recipe sync` will consume.
func (h *RecipesHandlers) HandleAssigned(w http.ResponseWriter, r *http.Request) {
	user := h.userFromCtx(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rows, err := h.svc.AssignedForUser(user.ID)
	if err != nil {
		slog.Warn("recipes assigned", "user", user.ID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assigned": rows})
}

// HandleRecipeByPath routes /api/recipes/{slug}.
func (h *RecipesHandlers) HandleRecipeByPath(w http.ResponseWriter, r *http.Request) {
	user := h.userFromCtx(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/recipes/")
	parts := strings.Split(rest, "/")
	slug := parts[0]
	if slug == "" {
		writeJSONError(w, http.StatusNotFound, "missing slug")
		return
	}
	recipe, err := h.store.GetRecipeBySlug(slug)
	if err != nil {
		slog.Warn("recipes lookup", "slug", slug, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if recipe == nil {
		writeJSONError(w, http.StatusNotFound, "recipe not found")
		return
	}

	// Read-side ACL: non-admins see public + their own assignments only.
	if r.Method == http.MethodGet && len(parts) == 1 && user.Role != "admin" {
		if !h.userCanRead(user, recipe) {
			writeJSONError(w, http.StatusNotFound, "recipe not found")
			return
		}
	}

	switch len(parts) {
	case 1:
		// /api/recipes/{slug}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, recipe)
		case http.MethodPatch:
			// PATCH is treated as an update-in-place — same capability as POST.
			if !requireCapability(w, r, user, h.apiKeyFromCtx(r.Context()), "recipes:write") {
				return
			}
			h.update(w, r, recipe)
		case http.MethodDelete:
			// DELETE stays admin-only. Future enhancement: gate behind
			// `recipes:yank` once we add an upload-ownership column.
			if user.Role != "admin" {
				writeJSONError(w, http.StatusForbidden, "admin access required")
				return
			}
			h.delete(w, r, recipe)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case 2:
		// /api/recipes/{slug}/assignments — admin only
		if parts[1] != "assignments" {
			writeJSONError(w, http.StatusNotFound, "unknown subresource")
			return
		}
		if user.Role != "admin" {
			writeJSONError(w, http.StatusForbidden, "admin access required")
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.listAssignments(w, recipe)
		case http.MethodPost:
			h.assignRecipe(w, r, recipe, user.ID)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case 3:
		// /api/recipes/{slug}/assignments/{username} — DELETE only (admin)
		if parts[1] != "assignments" {
			writeJSONError(w, http.StatusNotFound, "unknown subresource")
			return
		}
		if user.Role != "admin" {
			writeJSONError(w, http.StatusForbidden, "admin access required")
			return
		}
		username := parts[2]
		if username == "" {
			writeJSONError(w, http.StatusBadRequest, "missing username")
			return
		}
		if r.Method != http.MethodDelete {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.unassignRecipe(w, recipe, username)
	default:
		writeJSONError(w, http.StatusNotFound, "unknown subresource")
	}
}

// listForUser returns recipes visible to the user: admins see all; non-admins
// see public + their own assignments. Yanked recipes are filtered for non-admins.
func (h *RecipesHandlers) list(w http.ResponseWriter, user *store.User) {
	var (
		out []*store.SetupRecipe
		err error
	)
	if user.Role == "admin" {
		out, err = h.store.ListRecipes()
	} else {
		var assigned []*store.AssignedRecipe
		assigned, err = h.store.AssignedForUser(user.ID)
		if err == nil {
			out = make([]*store.SetupRecipe, 0, len(assigned))
			for _, a := range assigned {
				out = append(out, a.Recipe)
			}
		}
	}
	if err != nil {
		slog.Warn("recipes list", "user", user.ID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recipes": out})
}

// createBody is the wire shape for POST /api/recipes.
type createBody struct {
	Slug        string          `json:"slug"`
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	RecipeType  string          `json:"recipe_type"`
	RecipeData  json.RawMessage `json:"recipe_data"`
	Visibility  string          `json:"visibility"`
}

func (h *RecipesHandlers) create(w http.ResponseWriter, r *http.Request, uploaderID string) {
	r.Body = http.MaxBytesReader(w, r.Body, recipesBodyLimit)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "body exceeds 64 KiB limit")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	var in createBody
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := h.svc.Create(&recipes.CreateInput{
		Slug:        in.Slug,
		DisplayName: in.DisplayName,
		Description: in.Description,
		RecipeType:  in.RecipeType,
		RecipeData:  in.RecipeData,
		Visibility:  in.Visibility,
		CreatedBy:   uploaderID,
	})
	if err != nil {
		switch {
		case errors.Is(err, recipes.ErrInvalidRecipe):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, store.ErrRecipeSlugConflict):
			writeJSONError(w, http.StatusConflict, "slug already exists")
		default:
			slog.Warn("recipes create", "slug", in.Slug, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// updateBody is the wire shape for PATCH /api/recipes/{slug}.
type updateBody struct {
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	Visibility  string          `json:"visibility"`  // empty = preserve
	RecipeData  json.RawMessage `json:"recipe_data"` // empty = preserve
}

func (h *RecipesHandlers) update(w http.ResponseWriter, r *http.Request, recipe *store.SetupRecipe) {
	r.Body = http.MaxBytesReader(w, r.Body, recipesBodyLimit)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "body exceeds 64 KiB limit")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	var in updateBody
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := h.svc.Update(recipe.ID, &recipes.UpdateInput{
		DisplayName: in.DisplayName,
		Description: in.Description,
		Visibility:  in.Visibility,
		RecipeData:  in.RecipeData,
	})
	if err != nil {
		switch {
		case errors.Is(err, recipes.ErrInvalidRecipe):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, recipes.ErrRecipeNotFound):
			writeJSONError(w, http.StatusNotFound, "recipe not found")
		default:
			slog.Warn("recipes update", "slug", recipe.Slug, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *RecipesHandlers) delete(w http.ResponseWriter, r *http.Request, recipe *store.SetupRecipe) {
	hard := r.URL.Query().Get("hard") == "true"
	if hard {
		if err := h.store.DeleteRecipe(recipe.ID); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.store.YankRecipe(recipe.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"yanked": true})
}

// userCanRead implements the visibility check for non-admin GETs. Mirrors
// AssignedForUser's WHERE clause: yanked hidden, public visible, restricted
// only with explicit assignment.
func (h *RecipesHandlers) userCanRead(user *store.User, recipe *store.SetupRecipe) bool {
	if recipe.YankedAt != nil {
		return false
	}
	if recipe.Visibility == "public" {
		return true
	}
	assigns, err := h.store.ListAssignmentsForRecipe(recipe.ID)
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

// listAssignments returns the existing grants for a recipe. Admin-only at the
// route level — all callers reaching here have already passed the admin check.
func (h *RecipesHandlers) listAssignments(w http.ResponseWriter, recipe *store.SetupRecipe) {
	rows, err := h.store.ListAssignmentsForRecipe(recipe.ID)
	if err != nil {
		slog.Warn("recipes list assignments", "slug", recipe.Slug, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assignments": rows})
}

// assignRecipe grants a user access to a restricted recipe. Body shape:
//
//	{"username":"alice"}
//
// Idempotent: re-assigning replaces the prior grant.
func (h *RecipesHandlers) assignRecipe(w http.ResponseWriter, r *http.Request, recipe *store.SetupRecipe, adminID string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	var in assignBody
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(in.Username) == "" {
		writeJSONError(w, http.StatusBadRequest, "username is required")
		return
	}
	target, err := h.users.GetByUsername(in.Username)
	if err != nil {
		slog.Warn("recipes assign user lookup", "username", in.Username, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if target == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	a := &store.SetupRecipeAssignment{RecipeID: recipe.ID, UserID: target.ID}
	if adminID != "" {
		a.AssignedBy = &adminID
	}
	if err := h.store.AssignRecipe(a); err != nil {
		slog.Warn("recipes assign", "slug", recipe.Slug, "user", in.Username, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// unassignRecipe revokes a grant. Returns 404 on unknown username so a typo
// doesn't pass silently as an idempotent no-op.
func (h *RecipesHandlers) unassignRecipe(w http.ResponseWriter, recipe *store.SetupRecipe, username string) {
	target, err := h.users.GetByUsername(username)
	if err != nil {
		slog.Warn("recipes unassign user lookup", "username", username, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if target == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	if err := h.store.UnassignRecipe(recipe.ID, target.ID); err != nil {
		slog.Warn("recipes unassign", "slug", recipe.Slug, "user", username, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
