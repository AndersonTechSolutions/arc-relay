package web

import (
	"fmt"
	"sort"
	"time"

	"github.com/comma-compliance/arc-relay/internal/store"
)

// activityStores bundles the three stores needed to build an activity feed.
type activityStores struct {
	skills  *store.SkillStore
	recipes *store.SetupRecipeStore
	users   *store.UserStore
}

// ActivityEvent is one entry in the dashboard activity feed.
type ActivityEvent struct {
	Kind      string
	Timestamp time.Time
	Actor     string
	Subject   string
	URL       string
	Severity  string
	SkillID   string
	RecipeID  string
	UserID    string
}

// Pill returns a display emoji for the event kind.
// Collisions are intentional: skill_yanked and recipe_yanked both return "🚫",
// and skill_assigned and recipe_assigned both return "🔑". The Subject field
// provides disambiguation in the UI.
func (e ActivityEvent) Pill() string {
	switch e.Kind {
	case "skill_published":
		return "📦"
	case "skill_yanked":
		return "🚫"
	case "skill_drift":
		return "⚠️"
	case "skill_assigned":
		return "🔑"
	case "recipe_published":
		return "📋"
	case "recipe_yanked":
		return "🚫"
	case "recipe_assigned":
		return "🔑"
	case "user_joined":
		return "👤"
	case "key_issued":
		return "🗝️"
	default:
		return ""
	}
}

// buildActivityFeed assembles a time-sorted, viewer-scoped activity feed from
// all nine event sources. Errors from individual sources are silently ignored
// (best-effort feed). Results are capped at limit items, newest first.
func buildActivityFeed(stores activityStores, viewer *store.User, limit int) []ActivityEvent {
	if limit <= 0 {
		return nil
	}

	since := time.Now().AddDate(0, 0, -30)

	// Use a generous per-source fetch limit so filtering doesn't starve the result.
	fetchLimit := limit * 10
	if fetchLimit < 100 {
		fetchLimit = 100
	}

	isAdmin := viewer.Role == "admin"

	// Pre-compute accessible skill/recipe IDs for non-admins (O(1) lookup).
	// AssignedForUser returns both public AND restricted assigned items, so the
	// maps may contain public IDs. That is harmless: the filter checks
	// r.Visibility != "public" first and short-circuits before reaching the map
	// lookup for any public item, making the redundant entries a no-op.
	accessibleSkillIDs := map[string]bool{}
	accessibleRecipeIDs := map[string]bool{}
	if !isAdmin {
		if assigned, err := stores.skills.AssignedForUser(viewer.ID); err == nil {
			for _, a := range assigned {
				accessibleSkillIDs[a.Skill.ID] = true
			}
		}
		if assigned, err := stores.recipes.AssignedForUser(viewer.ID); err == nil {
			for _, a := range assigned {
				accessibleRecipeIDs[a.Recipe.ID] = true
			}
		}
	}

	var events []ActivityEvent

	// --- skill_published ---
	if versions, err := stores.skills.RecentSkillVersions(fetchLimit, since); err == nil {
		for _, r := range versions {
			if !isAdmin && r.Visibility != "public" && !accessibleSkillIDs[r.SkillID] {
				continue
			}
			actor := ""
			if r.UploadedBy != nil {
				actor = *r.UploadedBy
			}
			events = append(events, ActivityEvent{
				Kind:      "skill_published",
				Timestamp: r.UploadedAt,
				Actor:     actor,
				Subject:   fmt.Sprintf("%s@%s", r.Slug, r.Version),
				URL:       fmt.Sprintf("/skills/%s", r.Slug),
				SkillID:   r.SkillID,
			})
		}
	}

	// --- skill_yanked ---
	// Note: r.Visibility reflects the current value at query time, not the value
	// at yank time. This is acceptable for a best-effort feed (eventual consistency).
	if yanks, err := stores.skills.RecentSkillYanks(fetchLimit, since); err == nil {
		for _, r := range yanks {
			if !isAdmin && r.Visibility != "public" && !accessibleSkillIDs[r.SkillID] {
				continue
			}
			events = append(events, ActivityEvent{
				Kind:      "skill_yanked",
				Timestamp: r.YankedAt,
				Subject:   r.Slug,
				URL:       fmt.Sprintf("/skills/%s", r.Slug),
				SkillID:   r.SkillID,
			})
		}
	}

	// --- skill_drift ---
	if drifts, err := stores.skills.RecentDrift(fetchLimit, since); err == nil {
		for _, r := range drifts {
			if !isAdmin && r.Visibility != "public" && !accessibleSkillIDs[r.SkillID] {
				continue
			}
			sev := ""
			if r.Severity != nil {
				sev = *r.Severity
			}
			events = append(events, ActivityEvent{
				Kind:      "skill_drift",
				Timestamp: r.DriftAt,
				Subject:   r.Slug,
				URL:       fmt.Sprintf("/skills/%s", r.Slug),
				Severity:  sev,
				SkillID:   r.SkillID,
			})
		}
	}

	// --- skill_assigned ---
	if assignments, err := stores.skills.RecentSkillAssignments(fetchLimit, since); err == nil {
		for _, r := range assignments {
			if !isAdmin && r.UserID != viewer.ID {
				continue
			}
			events = append(events, ActivityEvent{
				Kind:      "skill_assigned",
				Timestamp: r.AssignedAt,
				Subject:   r.Slug,
				URL:       fmt.Sprintf("/skills/%s", r.Slug),
				SkillID:   r.SkillID,
				UserID:    r.UserID,
			})
		}
	}

	// --- recipe_published ---
	if recipes, err := stores.recipes.RecentRecipes(fetchLimit, since); err == nil {
		for _, r := range recipes {
			if !isAdmin && r.Visibility != "public" && !accessibleRecipeIDs[r.RecipeID] {
				continue
			}
			actor := ""
			if r.CreatedBy != nil {
				actor = *r.CreatedBy
			}
			events = append(events, ActivityEvent{
				Kind:      "recipe_published",
				Timestamp: r.CreatedAt,
				Actor:     actor,
				Subject:   r.Slug,
				URL:       fmt.Sprintf("/recipes/%s", r.Slug),
				RecipeID:  r.RecipeID,
			})
		}
	}

	// --- recipe_yanked ---
	// Note: r.Visibility reflects the current value at query time, not the value
	// at yank time. This is acceptable for a best-effort feed (eventual consistency).
	if yanks, err := stores.recipes.RecentRecipeYanks(fetchLimit, since); err == nil {
		for _, r := range yanks {
			if !isAdmin && r.Visibility != "public" && !accessibleRecipeIDs[r.RecipeID] {
				continue
			}
			events = append(events, ActivityEvent{
				Kind:      "recipe_yanked",
				Timestamp: r.YankedAt,
				Subject:   r.Slug,
				URL:       fmt.Sprintf("/recipes/%s", r.Slug),
				RecipeID:  r.RecipeID,
			})
		}
	}

	// --- recipe_assigned ---
	if assignments, err := stores.recipes.RecentRecipeAssignments(fetchLimit, since); err == nil {
		for _, r := range assignments {
			if !isAdmin && r.UserID != viewer.ID {
				continue
			}
			events = append(events, ActivityEvent{
				Kind:      "recipe_assigned",
				Timestamp: r.AssignedAt,
				Subject:   r.Slug,
				URL:       fmt.Sprintf("/recipes/%s", r.Slug),
				RecipeID:  r.RecipeID,
				UserID:    r.UserID,
			})
		}
	}

	// --- user_joined (admin only) ---
	if isAdmin {
		if users, err := stores.users.RecentUsers(fetchLimit, since); err == nil {
			for _, r := range users {
				events = append(events, ActivityEvent{
					Kind:      "user_joined",
					Timestamp: r.CreatedAt,
					Actor:     r.Username,
					Subject:   r.Username,
					URL:       "/users",
					UserID:    r.UserID,
				})
			}
		}
	}

	// --- key_issued (admin only) ---
	if isAdmin {
		if keys, err := stores.users.RecentAPIKeys(fetchLimit, since); err == nil {
			for _, r := range keys {
				events = append(events, ActivityEvent{
					Kind:      "key_issued",
					Timestamp: r.CreatedAt,
					Actor:     r.Username,
					Subject:   r.KeyName,
					URL:       "/users",
					UserID:    r.UserID,
				})
			}
		}
	}

	// Sort DESC by Timestamp.
	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp.After(events[j].Timestamp)
	})

	// Cap at limit.
	if len(events) > limit {
		events = events[:limit]
	}

	return events
}
