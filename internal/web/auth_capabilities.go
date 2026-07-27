package web

import (
	"net/http"

	"github.com/comma-compliance/arc-relay/internal/store"
)

// requireCapability is the standard write-path gate for endpoints that should
// be reachable by:
//
//   - any admin user (role="admin"; web-login or admin API key), OR
//   - any API key that has been granted the named capability (e.g. a CI
//     server's key issued with capabilities=["skills:write"]).
//
// Returns true when the request should proceed. On false, this function has
// already written a 403 JSON response with a hint naming the missing
// capability — callers should NOT also write to w.
//
// `key` is nil for session-cookie auth (web admin login). That path is
// allowed only when `user.Role == "admin"`. A capability cannot be
// granted to a session-cookie user without going through an API key.
//
// The error body intentionally names the capability so a CI operator
// reading their relay's response can hand it to whoever issues keys
// without further round-trips. Internal "is this user admin" gating is
// implementation detail and not surfaced.
func requireCapability(w http.ResponseWriter, _ *http.Request, user *store.User, key *store.APIKey, cap string) bool {
	if hasCapability(user, key, cap) {
		return true
	}
	writeJSONError(w, http.StatusForbidden, "missing capability: "+cap+" — admin must issue an API key with this capability before this endpoint will accept the request")
	return false
}

// hasCapability answers the same question as requireCapability without writing
// a response. Use it where a capability governs one *field* of an otherwise
// permitted request — e.g. a publish-capable key may upload a version, but
// choosing ?visibility= needs skills:admin — so the handler can reject just
// that field with a specific message instead of failing the whole route.
func hasCapability(user *store.User, key *store.APIKey, cap string) bool {
	if user != nil && user.Role == "admin" {
		return true
	}
	return key != nil && key.HasCapability(cap)
}
