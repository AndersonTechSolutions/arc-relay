package store_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
)

func TestAPIKeyHasCapability(t *testing.T) {
	cases := []struct {
		name string
		caps []string
		ask  string
		want bool
	}{
		{"empty", nil, "skills:write", false},
		{"present", []string{"skills:write", "recipes:write"}, "skills:write", true},
		{"absent", []string{"recipes:write"}, "skills:write", false},
		{"case-sensitive", []string{"skills:Write"}, "skills:write", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := &store.APIKey{Capabilities: tc.caps}
			if got := k.HasCapability(tc.ask); got != tc.want {
				t.Errorf("HasCapability(%q) = %v, want %v", tc.ask, got, tc.want)
			}
		})
	}

	t.Run("nil receiver", func(t *testing.T) {
		var k *store.APIKey
		if k.HasCapability("skills:write") {
			t.Error("nil receiver should return false")
		}
	})
}

func TestCreateAPIKeyPersistsCapabilities(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)

	user, err := users.Create("capuser", "pass", "user")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Input deliberately unsorted; normalize sorts on the way in.
	caps := []string{"skills:write", "recipes:write"}
	wantCaps := []string{"recipes:write", "skills:write"}
	rawKey, ak, err := users.CreateAPIKey(user.ID, "publisher key", nil, caps)
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if !reflect.DeepEqual(ak.Capabilities, wantCaps) {
		t.Errorf("returned APIKey.Capabilities = %v, want %v (sorted)", ak.Capabilities, wantCaps)
	}

	// Round-trip via ValidateAPIKey
	_, validated, err := users.ValidateAPIKey(rawKey)
	if err != nil {
		t.Fatalf("ValidateAPIKey() error = %v", err)
	}
	if validated == nil {
		t.Fatal("ValidateAPIKey returned nil api_key")
	}
	if !reflect.DeepEqual(validated.Capabilities, wantCaps) {
		t.Errorf("validated APIKey.Capabilities = %v, want %v", validated.Capabilities, wantCaps)
	}
	if !validated.HasCapability("skills:write") {
		t.Error("validated key should have skills:write")
	}
	if validated.HasCapability("users:invite") {
		t.Error("validated key should not have users:invite")
	}
}

func TestCreateAPIKeyNormalizesCapabilities(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)

	user, _ := users.Create("normuser", "pass", "user")

	// Mixed-up input: dupes, whitespace, empty strings, unsorted.
	in := []string{"recipes:write", "skills:write", "", "  skills:write  ", "skills:write"}
	want := []string{"recipes:write", "skills:write"} // deduped, sorted, trimmed, no empties.

	_, ak, err := users.CreateAPIKey(user.ID, "norm key", nil, in)
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if !reflect.DeepEqual(ak.Capabilities, want) {
		t.Errorf("normalized capabilities = %v, want %v", ak.Capabilities, want)
	}
}

func TestCreateAPIKeyEmptyCapabilities(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)

	user, _ := users.Create("emptyuser", "pass", "user")

	// nil input, empty input — both should produce an empty (non-nil) slice
	// and an empty-array JSON column on disk.
	for _, in := range [][]string{nil, {}} {
		rawKey, ak, err := users.CreateAPIKey(user.ID, "k", nil, in)
		if err != nil {
			t.Fatalf("CreateAPIKey() error = %v (input=%v)", err, in)
		}
		if len(ak.Capabilities) != 0 {
			t.Errorf("len(returned caps) = %d, want 0 (input=%v)", len(ak.Capabilities), in)
		}
		_, validated, err := users.ValidateAPIKey(rawKey)
		if err != nil {
			t.Fatalf("ValidateAPIKey() error = %v", err)
		}
		if len(validated.Capabilities) != 0 {
			t.Errorf("len(validated caps) = %d, want 0 (input=%v)", len(validated.Capabilities), in)
		}
	}
}

func TestSupportedCapabilitiesShape(t *testing.T) {
	// Sanity check on the canonical list — ensures someone doesn't
	// accidentally introduce a bad string or drop an entry keys were issued
	// with. The original MVP four must stay listed even though skills:write
	// and recipes:write are now legacy aliases: admins need to see what an
	// already-issued key holds.
	want := map[string]bool{
		"skills:write":  true,
		"skills:yank":   true,
		"recipes:write": true,
		"recipes:yank":  true,
	}
	got := map[string]bool{}
	for _, c := range store.SupportedCapabilities {
		got[c] = true
	}
	for c := range want {
		if !got[c] {
			t.Errorf("SupportedCapabilities missing %q", c)
		}
	}
	// Deliberately not an exact count: the list documents itself as
	// "additive growth only", so pinning the length would fail every time a
	// capability is legitimately added. Well-formedness is the real check.
	for _, c := range store.SupportedCapabilities {
		area, verb, ok := strings.Cut(c, ":")
		if !ok || area == "" || verb == "" || strings.ToLower(c) != c {
			t.Errorf("capability %q is malformed; want lowercase area:verb", c)
		}
	}
}
