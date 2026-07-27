package store

import (
	"slices"
	"testing"
)

// A key's effective powers are what matter, not the literal strings stored.
func TestHasCapability_LegacyAliasGrantsPublishOnly(t *testing.T) {
	legacy := &APIKey{Capabilities: []string{"skills:write"}}

	if !legacy.HasCapability("skills:publish") {
		t.Error("skills:write must still grant skills:publish — existing CI keys would break")
	}
	// The whole point of the split: a key issued before it must not retain the
	// power to choose who receives a skill, or to aim the checker's clone.
	if legacy.HasCapability("skills:admin") {
		t.Error("skills:write must NOT grant skills:admin (visibility + upstream)")
	}
	if legacy.HasCapability("skills:yank") {
		t.Error("skills:write must NOT grant skills:yank")
	}

	legacyRecipes := &APIKey{Capabilities: []string{"recipes:write"}}
	if !legacyRecipes.HasCapability("recipes:publish") {
		t.Error("recipes:write must still grant recipes:publish")
	}
	if legacyRecipes.HasCapability("recipes:admin") {
		t.Error("recipes:write must NOT grant recipes:admin")
	}
}

func TestHasCapability_ExactAndNil(t *testing.T) {
	k := &APIKey{Capabilities: []string{"skills:admin"}}
	if !k.HasCapability("skills:admin") {
		t.Error("exact match must hold")
	}
	if k.HasCapability("skills:publish") {
		t.Error("skills:admin must not imply publish — grants are explicit, not hierarchical")
	}
	var nilKey *APIKey
	if nilKey.HasCapability("skills:publish") {
		t.Error("nil key must grant nothing")
	}
}

// Every declared capability needs an enforcement site, or the list misleads
// whoever ticks the boxes. skills:yank and recipes:yank were previously
// declared and never checked.
func TestSupportedCapabilitiesCoverBothHalves(t *testing.T) {
	for _, want := range []string{
		"skills:publish", "skills:admin", "skills:yank",
		"recipes:publish", "recipes:admin", "recipes:yank",
	} {
		if !slices.Contains(SupportedCapabilities, want) {
			t.Errorf("SupportedCapabilities missing %q", want)
		}
	}
	// Legacy names stay listed so admins can see what an old key holds.
	for _, legacy := range []string{"skills:write", "recipes:write"} {
		if !slices.Contains(SupportedCapabilities, legacy) {
			t.Errorf("legacy capability %q should remain listed for existing keys", legacy)
		}
	}
}
