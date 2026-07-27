package server_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every route under /api/ and /mcp/ is reachable from the public internet.
//
// Cloudflare Access sits in front of arc-relay, but the app's own bypass
// entries exempt those prefixes — deliberately, because MCP clients and
// arc-sync send bearer tokens and cannot complete an interactive SSO
// redirect. The practical consequence is that for these two prefixes the
// relay's own authentication is the *only* gate. A route registered without
// an auth wrapper is world-readable the moment it deploys, with no error and
// no alert.
//
// This test enumerates route registrations from source and fails when a route
// under those prefixes is neither wrapped in an auth middleware nor listed in
// unauthenticatedByDesign below. It is deliberately a source-level check
// rather than a behavioural one: constructing the full server needs a
// database, stores and config, and the failure being guarded against is a
// registration that forgets the wrapper — which is visible in the source.
//
// If this fails on a route you added, the question to answer is not "how do I
// silence this" but "should this endpoint be readable by anyone on the
// internet".
var routeSourceFiles = []string{
	"../server/http.go",
	"../web/handlers.go",
}

// authWrappers are the middleware names that constitute server-side
// authentication. A registration whose handler expression mentions one of
// these is considered gated.
var authWrappers = []string{
	"apiAuth",      // API-key bearer auth (server.APIKeyAuth)
	"APIKeyAuth",   // the underlying constructor, in case it is used directly
	"MCPAuth",      // API key OR OAuth token, for /mcp/*
	"requireAuth",  // web session auth (redirects to the relay's own login)
	"requireAdmin", // stricter form of the above
}

// unauthenticatedByDesign is the allowlist. Each entry must be reachable
// before the caller possesses a credential, so no wrapper is possible. Adding
// to this list is a security decision — state why.
var unauthenticatedByDesign = map[string]string{
	// Device-authorisation grant. The CLI calls these before it has a key.
	// Neither mints anything on its own: /api/auth/device only creates a
	// pending request, and /api/auth/device/token returns a key only once the
	// request has been approved at /auth/device — a browser page that requires
	// a relay session AND is not in any Cloudflare bypass entry, so it also
	// requires SSO. Both are per-IP rate limited.
	"/api/auth/device":       "device-flow start; approval happens at /auth/device behind session + SSO",
	"/api/auth/device/token": "device-flow poll; returns a key only for an already-approved request",

	// Invite redemption. The bearer of a valid invite is by definition not yet
	// authenticated. The token is a UUIDv4 stored as a SHA-256 hash, consumed
	// atomically (UPDATE ... WHERE status='pending' AND expires_at > now, then
	// RowsAffected==1), expiring, and per-IP rate limited.
	"/api/auth/invite": "invite redemption; single-use hashed token, atomic consume, expiring, rate limited",
}

func TestPublicSurfaceRoutesAreAuthenticated(t *testing.T) {
	var unguarded []string
	seen := map[string]bool{}

	for _, file := range routeSourceFiles {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			route, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if !strings.HasPrefix(route, "/api/") && !strings.HasPrefix(route, "/mcp/") {
				return true
			}
			seen[route] = true
			if _, allowed := unauthenticatedByDesign[route]; allowed {
				return true
			}
			// Render the handler argument and look for an auth wrapper.
			var sb strings.Builder
			renderExpr(&sb, call.Args[1])
			handler := sb.String()
			for _, w := range authWrappers {
				if strings.Contains(handler, w) {
					return true
				}
			}
			unguarded = append(unguarded, route+"  (handler: "+handler+")")
			return true
		})
	}

	if len(seen) == 0 {
		t.Fatal("parsed no /api/ or /mcp/ routes — the source layout changed and this test is no longer checking anything")
	}

	sort.Strings(unguarded)
	for _, r := range unguarded {
		t.Errorf("route is reachable from the internet with no server-side auth: %s", r)
	}
	if len(unguarded) > 0 {
		t.Logf("Cloudflare Access bypasses /api/* and /mcp/*, so these are public. " +
			"Wrap the handler in apiAuth/MCPAuth/requireAuth, or add it to " +
			"unauthenticatedByDesign with a justification.")
	}
}

// Entries in the allowlist must still exist. A stale entry silently widens the
// allowlist for whatever route later takes that path.
func TestUnauthenticatedAllowlistHasNoStaleEntries(t *testing.T) {
	found := map[string]bool{}
	for _, file := range routeSourceFiles {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 1 {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); !ok ||
				(sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
				return true
			}
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if r, err := strconv.Unquote(lit.Value); err == nil {
					found[r] = true
				}
			}
			return true
		})
	}
	for route := range unauthenticatedByDesign {
		if !found[route] {
			t.Errorf("allowlisted route %q is no longer registered — remove it so it cannot "+
				"pre-approve a future route on the same path", route)
		}
	}
}

// renderExpr writes a flat textual form of an expression. Enough to spot a
// wrapper call without pulling in go/printer.
func renderExpr(sb *strings.Builder, e ast.Expr) {
	switch v := e.(type) {
	case *ast.Ident:
		sb.WriteString(v.Name)
	case *ast.SelectorExpr:
		renderExpr(sb, v.X)
		sb.WriteString(".")
		sb.WriteString(v.Sel.Name)
	case *ast.CallExpr:
		renderExpr(sb, v.Fun)
		sb.WriteString("(")
		for i, a := range v.Args {
			if i > 0 {
				sb.WriteString(",")
			}
			renderExpr(sb, a)
		}
		sb.WriteString(")")
	case *ast.BasicLit:
		sb.WriteString(v.Value)
	default:
		sb.WriteString("?")
	}
}
