package web

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
)

// newActivityStores wires up all three stores against the test DB.
func newActivityStores(t *testing.T) (activityStores, *store.SkillStore, *store.SetupRecipeStore, *store.UserStore) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	skills := store.NewSkillStore(db)
	recipes := store.NewSetupRecipeStore(db)
	users := store.NewUserStore(db)
	return activityStores{skills: skills, recipes: recipes, users: users}, skills, recipes, users
}

// TestBuildActivityFeed_MergesSourcesByTimestamp verifies that events from
// multiple sources are merged and returned sorted DESC by Timestamp.
func TestBuildActivityFeed_MergesSourcesByTimestamp(t *testing.T) {
	stores, skills, _, users := newActivityStores(t)

	// Create a public skill and publish a version → generates skill_published event.
	sk := &store.Skill{Slug: "merge-test-skill", DisplayName: "Merge Test Skill", Description: "desc", Visibility: "public"}
	if err := skills.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	sv := &store.SkillVersion{
		SkillID:       sk.ID,
		Version:       "1.0.0",
		ArchivePath:   "path/merge-test.tar.gz",
		ArchiveSize:   1024,
		ArchiveSHA256: "abc123",
		Manifest:      json.RawMessage(`{}`),
	}
	if err := skills.CreateVersion(sv); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}

	// Create a user → generates user_joined event (visible to admins).
	admin, err := users.Create("merge-admin", "pw", "admin")
	if err != nil {
		t.Fatalf("Create admin: %v", err)
	}

	feed := buildActivityFeed(stores, admin, 50)

	// We expect at least 2 events: skill_published + user_joined.
	if len(feed) < 2 {
		t.Fatalf("expected >= 2 events, got %d: %+v", len(feed), feed)
	}

	// Verify sorted DESC (each event's Timestamp should be >= the next one's).
	for i := 1; i < len(feed); i++ {
		if feed[i-1].Timestamp.Before(feed[i].Timestamp) {
			t.Errorf("events not sorted DESC: event[%d] (%v) is before event[%d] (%v)",
				i-1, feed[i-1].Timestamp, i, feed[i].Timestamp)
		}
	}

	// Verify we have both expected kinds somewhere in the feed.
	kinds := map[string]bool{}
	for _, e := range feed {
		kinds[e.Kind] = true
	}
	if !kinds["skill_published"] {
		t.Error("expected skill_published event in feed")
	}
	if !kinds["user_joined"] {
		t.Error("expected user_joined event in feed")
	}
}

// TestBuildActivityFeed_NonAdminFiltersRestricted verifies that a non-admin
// sees skill_published for public skills but NOT for restricted skills that
// have no assignment.
func TestBuildActivityFeed_NonAdminFiltersRestricted(t *testing.T) {
	stores, skills, _, users := newActivityStores(t)

	// Public skill — non-admin should see this.
	public := &store.Skill{Slug: "public-skill", DisplayName: "Public", Description: "desc", Visibility: "public"}
	if err := skills.CreateSkill(public); err != nil {
		t.Fatalf("CreateSkill public: %v", err)
	}
	svPub := &store.SkillVersion{
		SkillID:       public.ID,
		Version:       "1.0.0",
		ArchivePath:   "path/public.tar.gz",
		ArchiveSize:   512,
		ArchiveSHA256: "pub123",
		Manifest:      json.RawMessage(`{}`),
	}
	if err := skills.CreateVersion(svPub); err != nil {
		t.Fatalf("CreateVersion public: %v", err)
	}

	// Restricted skill — non-admin should NOT see this (no assignment).
	restricted := &store.Skill{Slug: "restricted-skill", DisplayName: "Restricted", Description: "desc", Visibility: "restricted"}
	if err := skills.CreateSkill(restricted); err != nil {
		t.Fatalf("CreateSkill restricted: %v", err)
	}
	svRes := &store.SkillVersion{
		SkillID:       restricted.ID,
		Version:       "1.0.0",
		ArchivePath:   "path/restricted.tar.gz",
		ArchiveSize:   512,
		ArchiveSHA256: "res123",
		Manifest:      json.RawMessage(`{}`),
	}
	if err := skills.CreateVersion(svRes); err != nil {
		t.Fatalf("CreateVersion restricted: %v", err)
	}

	// Non-admin viewer.
	viewer, err := users.Create("restricted-viewer", "pw", "user")
	if err != nil {
		t.Fatalf("Create user: %v", err)
	}

	feed := buildActivityFeed(stores, viewer, 50)

	seenPublic := false
	seenRestricted := false
	for _, e := range feed {
		if e.Kind == "skill_published" {
			if e.SkillID == public.ID {
				seenPublic = true
			}
			if e.SkillID == restricted.ID {
				seenRestricted = true
			}
		}
	}

	if !seenPublic {
		t.Error("non-admin should see skill_published for public skill")
	}
	if seenRestricted {
		t.Error("non-admin should NOT see skill_published for restricted skill without assignment")
	}
}

// TestBuildActivityFeed_NonAdminHidesUserAndKeyEvents verifies that
// user_joined and key_issued events are hidden from non-admins but visible to admins.
func TestBuildActivityFeed_NonAdminHidesUserAndKeyEvents(t *testing.T) {
	stores, _, _, users := newActivityStores(t)

	// Create a user that will appear as a user_joined event.
	joined, err := users.Create("joined-user", "pw", "user")
	if err != nil {
		t.Fatalf("Create joined user: %v", err)
	}

	// Create an API key for that user → key_issued event.
	if _, _, err := users.CreateAPIKey(joined.ID, "my-key", nil, nil); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	// Non-admin viewer (a different user).
	viewer, err := users.Create("non-admin-viewer", "pw", "user")
	if err != nil {
		t.Fatalf("Create viewer: %v", err)
	}

	// Admin viewer.
	admin, err := users.Create("admin-viewer", "pw", "admin")
	if err != nil {
		t.Fatalf("Create admin: %v", err)
	}

	nonAdminFeed := buildActivityFeed(stores, viewer, 50)
	adminFeed := buildActivityFeed(stores, admin, 50)

	// Non-admin should not see user_joined or key_issued.
	for _, e := range nonAdminFeed {
		if e.Kind == "user_joined" {
			t.Errorf("non-admin should not see user_joined event, got: %+v", e)
		}
		if e.Kind == "key_issued" {
			t.Errorf("non-admin should not see key_issued event, got: %+v", e)
		}
	}

	// Admin should see both.
	adminKinds := map[string]bool{}
	for _, e := range adminFeed {
		adminKinds[e.Kind] = true
	}
	if !adminKinds["user_joined"] {
		t.Error("admin should see user_joined events")
	}
	if !adminKinds["key_issued"] {
		t.Error("admin should see key_issued events")
	}
}

// TestBuildActivityFeed_AssignmentScopedToSelf verifies that a non-admin
// viewer only sees skill assignment events for themselves, not for other users.
func TestBuildActivityFeed_AssignmentScopedToSelf(t *testing.T) {
	stores, skills, _, users := newActivityStores(t)

	// Create a restricted skill.
	sk := &store.Skill{Slug: "assign-scope-skill", DisplayName: "Assign Scope", Description: "desc", Visibility: "restricted"}
	if err := skills.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}

	// Create two non-admin users.
	userA, err := users.Create("scope-user-a", "pw", "user")
	if err != nil {
		t.Fatalf("Create userA: %v", err)
	}
	userB, err := users.Create("scope-user-b", "pw", "user")
	if err != nil {
		t.Fatalf("Create userB: %v", err)
	}

	// Assign the skill to both users.
	if err := skills.AssignSkill(&store.SkillAssignment{SkillID: sk.ID, UserID: userA.ID}); err != nil {
		t.Fatalf("AssignSkill userA: %v", err)
	}
	if err := skills.AssignSkill(&store.SkillAssignment{SkillID: sk.ID, UserID: userB.ID}); err != nil {
		t.Fatalf("AssignSkill userB: %v", err)
	}

	// Viewer is userA.
	feed := buildActivityFeed(stores, userA, 50)

	seenSelfAssign := false
	seenOtherAssign := false
	for _, e := range feed {
		if e.Kind == "skill_assigned" && e.SkillID == sk.ID {
			if e.UserID == userA.ID {
				seenSelfAssign = true
			}
			if e.UserID == userB.ID {
				seenOtherAssign = true
			}
		}
	}

	if !seenSelfAssign {
		t.Error("non-admin should see their own skill assignment event")
	}
	if seenOtherAssign {
		t.Error("non-admin should NOT see other users' skill assignment events")
	}
}

// TestBuildActivityFeed_LimitAnd30DayClamp verifies that the result is capped
// at the limit parameter when more events exist.
func TestBuildActivityFeed_LimitAnd30DayClamp(t *testing.T) {
	stores, skills, _, users := newActivityStores(t)

	admin, err := users.Create("limit-admin", "pw", "admin")
	if err != nil {
		t.Fatalf("Create admin: %v", err)
	}

	// Create a public skill and publish 5 versions.
	sk := &store.Skill{Slug: "limit-test-skill", DisplayName: "Limit Test", Description: "desc", Visibility: "public"}
	if err := skills.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	for i := 1; i <= 5; i++ {
		sv := &store.SkillVersion{
			SkillID:       sk.ID,
			Version:       fmt.Sprintf("1.0.%d", i),
			ArchivePath:   fmt.Sprintf("path/limit-%d.tar.gz", i),
			ArchiveSize:   int64(512 * i),
			ArchiveSHA256: fmt.Sprintf("hash%d", i),
			Manifest:      json.RawMessage(`{}`),
		}
		if err := skills.CreateVersion(sv); err != nil {
			t.Fatalf("CreateVersion %d: %v", i, err)
		}
	}

	// With limit=3, result should be capped at 3 even though more events exist.
	feed := buildActivityFeed(stores, admin, 3)

	if len(feed) > 3 {
		t.Errorf("expected at most 3 events with limit=3, got %d", len(feed))
	}

	// Verify the returned events are still sorted DESC.
	for i := 1; i < len(feed); i++ {
		if feed[i-1].Timestamp.Before(feed[i].Timestamp) {
			t.Errorf("events not sorted DESC at index %d/%d", i-1, i)
		}
	}

	// Sanity check: without a limit, we should see all 5 skill_published events
	// (plus the user_joined for the admin).
	feedFull := buildActivityFeed(stores, admin, 50)
	skillPublishedCount := 0
	for _, e := range feedFull {
		if e.Kind == "skill_published" {
			skillPublishedCount++
		}
	}
	if skillPublishedCount != 5 {
		t.Errorf("expected 5 skill_published events without limit, got %d", skillPublishedCount)
	}
}
