package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SetupRecipe is one row in `setup_recipes`. Recipe data is opaque JSON whose
// shape depends on RecipeType; the recipes service is responsible for shape
// validation. The store layer treats RecipeData as a pass-through.
type SetupRecipe struct {
	ID          string          `json:"id"`
	Slug        string          `json:"slug"`
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	RecipeType  string          `json:"recipe_type"`
	RecipeData  json.RawMessage `json:"recipe_data"`
	Visibility  string          `json:"visibility"`
	YankedAt    *time.Time      `json:"yanked_at,omitempty"`
	CreatedBy   *string         `json:"created_by,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// SetupRecipeAssignment grants a user access to a restricted recipe.
type SetupRecipeAssignment struct {
	RecipeID   string    `json:"recipe_id"`
	UserID     string    `json:"user_id"`
	AssignedBy *string   `json:"assigned_by,omitempty"`
	AssignedAt time.Time `json:"assigned_at"`
}

// AssignedRecipe is the row shape returned by AssignedForUser — the recipe
// itself plus per-user assignment metadata. Symmetric with AssignedSkill so
// future code can reuse rendering logic if it makes sense.
type AssignedRecipe struct {
	Recipe *SetupRecipe `json:"recipe"`
}

// ErrRecipeSlugConflict signals a UNIQUE constraint violation on recipes.slug.
var ErrRecipeSlugConflict = errors.New("recipe slug already exists")

// Allowed recipe types. Phase 0 ships only claude_plugin; other types are
// intentionally rejected at the CHECK constraint AND in code so that adding
// a new type is a deliberate two-step change (migration + here).
const RecipeTypeClaudePlugin = "claude_plugin"

// SetupRecipeStore is the persistence layer for setup_recipes and
// setup_recipe_assignments.
type SetupRecipeStore struct {
	db *DB
}

// NewSetupRecipeStore returns a SetupRecipeStore backed by db.
func NewSetupRecipeStore(db *DB) *SetupRecipeStore {
	return &SetupRecipeStore{db: db}
}

// CreateRecipe inserts a new recipe. Slug is validated by ValidateSlug (the
// shared regex from servers.go); recipe_type and visibility are validated by
// the CHECK constraints in the migration.
func (s *SetupRecipeStore) CreateRecipe(r *SetupRecipe) error {
	if err := ValidateSlug(r.Slug); err != nil {
		return err
	}
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	if r.Visibility == "" {
		r.Visibility = "restricted"
	}
	if r.Visibility != "public" && r.Visibility != "restricted" {
		return fmt.Errorf("invalid visibility %q", r.Visibility)
	}
	if r.RecipeType == "" {
		return fmt.Errorf("recipe_type is required")
	}
	if len(r.RecipeData) == 0 {
		r.RecipeData = json.RawMessage("{}")
	}
	now := time.Now()
	r.CreatedAt = now
	r.UpdatedAt = now

	_, err := s.db.Exec(`
		INSERT INTO setup_recipes
		    (id, slug, display_name, description, recipe_type, recipe_data,
		     visibility, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Slug, r.DisplayName, r.Description, r.RecipeType, string(r.RecipeData),
		r.Visibility, r.CreatedBy, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrRecipeSlugConflict
		}
		return fmt.Errorf("creating recipe: %w", err)
	}
	return nil
}

// GetRecipe returns a recipe by id, or (nil, nil) if not found.
func (s *SetupRecipeStore) GetRecipe(id string) (*SetupRecipe, error) {
	return s.scanRecipeRow(s.db.QueryRow(`
		SELECT id, slug, display_name, description, recipe_type, recipe_data,
		       visibility, yanked_at, created_by, created_at, updated_at
		FROM setup_recipes WHERE id = ?`, id))
}

// GetRecipeBySlug returns a recipe by slug, or (nil, nil) if not found.
func (s *SetupRecipeStore) GetRecipeBySlug(slug string) (*SetupRecipe, error) {
	return s.scanRecipeRow(s.db.QueryRow(`
		SELECT id, slug, display_name, description, recipe_type, recipe_data,
		       visibility, yanked_at, created_by, created_at, updated_at
		FROM setup_recipes WHERE slug = ?`, slug))
}

// ListRecipes returns all recipes ordered by slug. Yanked recipes are
// included; callers filter as needed.
func (s *SetupRecipeStore) ListRecipes() ([]*SetupRecipe, error) {
	rows, err := s.db.Query(`
		SELECT id, slug, display_name, description, recipe_type, recipe_data,
		       visibility, yanked_at, created_by, created_at, updated_at
		FROM setup_recipes ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("listing recipes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*SetupRecipe
	for rows.Next() {
		r, err := s.scanRecipe(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateRecipe replaces the mutable fields. Slug, ID, recipe_type are NOT
// changed by this method — slug renames need a separate ChangeSlug method
// (deferred until there's a concrete need); recipe_type is immutable
// because the data shape depends on it.
func (s *SetupRecipeStore) UpdateRecipe(id, displayName, description, visibility string, recipeData json.RawMessage) error {
	if visibility != "" && visibility != "public" && visibility != "restricted" {
		return fmt.Errorf("invalid visibility %q", visibility)
	}
	if len(recipeData) == 0 {
		recipeData = json.RawMessage("{}")
	}
	_, err := s.db.Exec(`
		UPDATE setup_recipes
		SET display_name = ?, description = ?,
		    visibility = COALESCE(NULLIF(?, ''), visibility),
		    recipe_data = ?, updated_at = ?
		WHERE id = ?`,
		displayName, description, visibility, string(recipeData), time.Now(), id,
	)
	if err != nil {
		return fmt.Errorf("updating recipe: %w", err)
	}
	return nil
}

// YankRecipe marks the recipe as yanked. Yanked recipes are hidden from
// listings and `assigned` queries but the row is preserved for audit.
func (s *SetupRecipeStore) YankRecipe(id string) error {
	now := time.Now()
	_, err := s.db.Exec(`UPDATE setup_recipes SET yanked_at = ?, updated_at = ? WHERE id = ?`,
		now, now, id)
	if err != nil {
		return fmt.Errorf("yanking recipe: %w", err)
	}
	return nil
}

// UnyankRecipe clears the yanked flag.
func (s *SetupRecipeStore) UnyankRecipe(id string) error {
	_, err := s.db.Exec(`UPDATE setup_recipes SET yanked_at = NULL, updated_at = ? WHERE id = ?`,
		time.Now(), id)
	if err != nil {
		return fmt.Errorf("unyanking recipe: %w", err)
	}
	return nil
}

// DeleteRecipe removes a recipe row. ON DELETE CASCADE cleans up assignments.
// Hard delete should be reserved for "never published" mistakes; prefer YankRecipe.
func (s *SetupRecipeStore) DeleteRecipe(id string) error {
	_, err := s.db.Exec(`DELETE FROM setup_recipes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting recipe: %w", err)
	}
	return nil
}

// AssignRecipe grants a user access to a restricted recipe. Upsert:
// re-assigning replaces the assigned_by + assigned_at fields.
func (s *SetupRecipeStore) AssignRecipe(a *SetupRecipeAssignment) error {
	if a.RecipeID == "" || a.UserID == "" {
		return fmt.Errorf("recipe_id and user_id are required")
	}
	a.AssignedAt = time.Now()
	_, err := s.db.Exec(`
		INSERT INTO setup_recipe_assignments (recipe_id, user_id, assigned_by, assigned_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(recipe_id, user_id) DO UPDATE SET
		    assigned_by = excluded.assigned_by,
		    assigned_at = excluded.assigned_at`,
		a.RecipeID, a.UserID, a.AssignedBy, a.AssignedAt,
	)
	if err != nil {
		return fmt.Errorf("assigning recipe: %w", err)
	}
	return nil
}

// UnassignRecipe revokes a single user's grant.
func (s *SetupRecipeStore) UnassignRecipe(recipeID, userID string) error {
	_, err := s.db.Exec(`DELETE FROM setup_recipe_assignments WHERE recipe_id = ? AND user_id = ?`,
		recipeID, userID)
	if err != nil {
		return fmt.Errorf("unassigning recipe: %w", err)
	}
	return nil
}

// ListAssignmentsForRecipe returns who has been granted access.
func (s *SetupRecipeStore) ListAssignmentsForRecipe(recipeID string) ([]*SetupRecipeAssignment, error) {
	rows, err := s.db.Query(`
		SELECT recipe_id, user_id, assigned_by, assigned_at
		FROM setup_recipe_assignments WHERE recipe_id = ? ORDER BY assigned_at`, recipeID)
	if err != nil {
		return nil, fmt.Errorf("listing recipe assignments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*SetupRecipeAssignment
	for rows.Next() {
		a := &SetupRecipeAssignment{}
		if err := rows.Scan(&a.RecipeID, &a.UserID, &a.AssignedBy, &a.AssignedAt); err != nil {
			return nil, fmt.Errorf("scanning recipe assignment: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AssignedForUser returns every recipe the user can install right now —
// public-and-not-yanked plus restricted recipes with an explicit assignment.
// Used by `arc-sync recipe sync` to compute the desired client state.
func (s *SetupRecipeStore) AssignedForUser(userID string) ([]*AssignedRecipe, error) {
	rows, err := s.db.Query(`
		SELECT r.id, r.slug, r.display_name, r.description, r.recipe_type, r.recipe_data,
		       r.visibility, r.yanked_at, r.created_by, r.created_at, r.updated_at
		FROM setup_recipes r
		LEFT JOIN setup_recipe_assignments a
		    ON a.recipe_id = r.id AND a.user_id = ?
		WHERE r.yanked_at IS NULL
		  AND (r.visibility = 'public' OR a.user_id IS NOT NULL)
		ORDER BY r.slug`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing assigned recipes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*AssignedRecipe
	for rows.Next() {
		r, err := s.scanRecipe(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, &AssignedRecipe{Recipe: r})
	}
	return out, rows.Err()
}

// scanRecipe scans one row off any rows.Scan-compatible iterator.
func (s *SetupRecipeStore) scanRecipe(scanner interface {
	Scan(dest ...any) error
}) (*SetupRecipe, error) {
	r := &SetupRecipe{}
	var data string
	var yankedAt sql.NullTime
	if err := scanner.Scan(&r.ID, &r.Slug, &r.DisplayName, &r.Description, &r.RecipeType,
		&data, &r.Visibility, &yankedAt, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, fmt.Errorf("scanning recipe: %w", err)
	}
	r.RecipeData = json.RawMessage(data)
	if yankedAt.Valid {
		t := yankedAt.Time
		r.YankedAt = &t
	}
	return r, nil
}

func (s *SetupRecipeStore) scanRecipeRow(row *sql.Row) (*SetupRecipe, error) {
	r, err := s.scanRecipe(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// RecentRecipeRow is returned by RecentRecipes.
type RecentRecipeRow struct {
	RecipeID   string
	Slug       string
	Visibility string
	CreatedBy  *string
	CreatedAt  time.Time
}

// RecentRecipes returns non-yanked recipes created within the since window, newest-first.
func (s *SetupRecipeStore) RecentRecipes(limit int, since time.Time) ([]*RecentRecipeRow, error) {
	rows, err := s.db.Query(`
		SELECT id, slug, visibility, created_by, created_at
		FROM setup_recipes
		WHERE created_at >= ? AND yanked_at IS NULL
		ORDER BY created_at DESC
		LIMIT ?`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("recent recipes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*RecentRecipeRow
	for rows.Next() {
		r := &RecentRecipeRow{}
		var by sql.NullString
		if err := rows.Scan(&r.RecipeID, &r.Slug, &r.Visibility, &by, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning recent recipe: %w", err)
		}
		if by.Valid {
			r.CreatedBy = &by.String
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentRecipeYankRow is returned by RecentRecipeYanks.
type RecentRecipeYankRow struct {
	RecipeID   string
	Slug       string
	Visibility string
	YankedAt   time.Time
}

// RecentRecipeYanks returns recipes yanked within the since window, newest-first.
func (s *SetupRecipeStore) RecentRecipeYanks(limit int, since time.Time) ([]*RecentRecipeYankRow, error) {
	rows, err := s.db.Query(`
		SELECT id, slug, visibility, yanked_at
		FROM setup_recipes
		WHERE yanked_at IS NOT NULL AND yanked_at >= ?
		ORDER BY yanked_at DESC
		LIMIT ?`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("recent recipe yanks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*RecentRecipeYankRow
	for rows.Next() {
		r := &RecentRecipeYankRow{}
		var yankedAt sql.NullTime
		if err := rows.Scan(&r.RecipeID, &r.Slug, &r.Visibility, &yankedAt); err != nil {
			return nil, fmt.Errorf("scanning recipe yank: %w", err)
		}
		if yankedAt.Valid {
			r.YankedAt = yankedAt.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentRecipeAssignmentRow is returned by RecentRecipeAssignments.
type RecentRecipeAssignmentRow struct {
	RecipeID   string
	Slug       string
	Visibility string
	UserID     string
	AssignedAt time.Time
}

// RecentRecipeAssignments returns recipe assignments made within the since window, newest-first,
// joined to slug and visibility for per-viewer filtering.
func (s *SetupRecipeStore) RecentRecipeAssignments(limit int, since time.Time) ([]*RecentRecipeAssignmentRow, error) {
	rows, err := s.db.Query(`
		SELECT ra.recipe_id, r.slug, r.visibility, ra.user_id, ra.assigned_at
		FROM setup_recipe_assignments ra
		JOIN setup_recipes r ON ra.recipe_id = r.id
		WHERE ra.assigned_at >= ?
		ORDER BY ra.assigned_at DESC
		LIMIT ?`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("recent recipe assignments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*RecentRecipeAssignmentRow
	for rows.Next() {
		r := &RecentRecipeAssignmentRow{}
		if err := rows.Scan(&r.RecipeID, &r.Slug, &r.Visibility, &r.UserID, &r.AssignedAt); err != nil {
			return nil, fmt.Errorf("scanning recipe assignment row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
