package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The route-enumeration meta-test (design §4.6): every route registered in
// router.go must carry the wrapper its authzRouteTable tier demands, and every
// tabled route must actually be registered. This is what makes authorization
// coverage UNAVOIDABLE for future routes — a new data route that is not added
// to the table (or is added under a weaker wrapper) fails always-on CI, so the
// omission becomes a visible, diffable table row in the PR instead of a silent
// hole (the BG-07/BG-11 root cause).
//
// It runs without a database: it statically parses router.go. The DB-backed
// behavioral proof of the same table is authz_matrix_integration_test.go.

// wrapperTier maps the outermost wrapper identifier of a registration to the
// tier it enforces. Handlers registered bare (no wrapper) are tierPublic.
var wrapperTier = map[string]authzTier{
	"authOwner":           tierOwner,
	"authOnly":            tierAuthed,
	"authAdmin":           tierAdminGate,
	"authAdminOnly":       tierAdminOnly,
	"leafViewer":          tierVisibility,
	"ed25519AuthRequired": tierVolunteerKey,
}

// registeredRoute is one mux registration parsed out of router.go.
type registeredRoute struct {
	method  string
	pattern string
	wrapper string // outermost identifier of the handler expression
	line    int
}

var muxRegistrationRe = regexp.MustCompile(`mux\.Handle(?:Func)?\(\s*"([A-Z]+) ([^"]+)"\s*,\s*([\w.]+)`)
var registrarCallRe = regexp.MustCompile(`([\w.]+(?:\([^)]*\))?)\.RegisterRoutes\(mux\)`)

func parseRouterRegistrations(t *testing.T) ([]registeredRoute, []string) {
	t.Helper()
	raw, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("failed to read router.go: %v", err)
	}
	src := string(raw)

	// lineOf maps a byte offset to a 1-based line number; commentedAt reports
	// whether that offset sits on a line-comment line (a registration may span
	// lines — mux.HandleFunc("...",\n\twrapper(...) — so matching runs over the
	// whole source, not per line).
	lineOf := func(off int) int { return 1 + strings.Count(src[:off], "\n") }
	commentedAt := func(off int) bool {
		lineStart := strings.LastIndexByte(src[:off], '\n') + 1
		return strings.HasPrefix(strings.TrimSpace(src[lineStart:off]), "//")
	}

	var routes []registeredRoute
	for _, idx := range muxRegistrationRe.FindAllStringSubmatchIndex(src, -1) {
		if commentedAt(idx[0]) {
			continue
		}
		routes = append(routes, registeredRoute{
			method:  src[idx[2]:idx[3]],
			pattern: src[idx[4]:idx[5]],
			wrapper: src[idx[6]:idx[7]],
			line:    lineOf(idx[0]),
		})
	}

	var registrars []string
	for _, idx := range registrarCallRe.FindAllStringSubmatchIndex(src, -1) {
		if commentedAt(idx[0]) {
			continue
		}
		registrars = append(registrars, src[idx[2]:idx[3]])
	}

	if len(routes) == 0 {
		t.Fatal("parsed no mux registrations out of router.go — the meta-test regex is broken")
	}
	return routes, registrars
}

// tierOf resolves a parsed registration to the tier its wrapper enforces.
func tierOf(r registeredRoute) authzTier {
	// The wrapper capture is the leading identifier of the handler expression:
	// a known auth wrapper ("authOwner"), or a bare handler reference
	// ("headHandler.HandleGetHeadInfo", "HealthHandler", "handleBatchStats",
	// "handleBrowserRegister") which enforces nothing → public.
	base := r.wrapper
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	if tier, ok := wrapperTier[base]; ok {
		return tier
	}
	return tierPublic
}

func TestAuthzMeta_EveryRegisteredRouteIsTabled(t *testing.T) {
	routes, _ := parseRouterRegistrations(t)

	for _, r := range routes {
		row := findAuthzRoute(r.method, r.pattern)
		if row == nil {
			t.Errorf("router.go:%d registers %s %s but authz_routes_test.go has no row for it — every data route must carry an explicit, reviewable authorization tier",
				r.line, r.method, r.pattern)
			continue
		}
		if got := tierOf(r); got != row.tier {
			t.Errorf("router.go:%d registers %s %s under wrapper %q (tier %q) but the authz table demands tier %q (%s)",
				r.line, r.method, r.pattern, r.wrapper, got, row.tier, row.item)
		}
	}
}

func TestAuthzMeta_EveryTabledRouteIsRegistered(t *testing.T) {
	routes, _ := parseRouterRegistrations(t)

	registered := make(map[string]bool, len(routes))
	for _, r := range routes {
		registered[r.method+" "+r.pattern] = true
	}

	for _, row := range authzRouteTable {
		if !registered[row.method+" "+row.pattern] {
			t.Errorf("authz table demands %s %s at tier %q (%s) but router.go does not register it — the route is either missing or still registered through a package RegisterRoutes without its auth wrapper",
				row.method, row.pattern, row.tier, row.item)
		}
	}
}

func TestAuthzMeta_OnlyPublicByDesignRegistrars(t *testing.T) {
	_, registrars := parseRouterRegistrations(t)

	for _, reg := range registrars {
		allowed := false
		for _, pub := range publicRegistrars {
			if strings.HasPrefix(reg, pub) {
				allowed = true
				break
			}
		}
		if !allowed {
			t.Errorf("router.go calls %s.RegisterRoutes(mux), which registers routes with NO auth wrapper, and %q is not on the public-by-design registrar allowlist — data routes must be registered individually in router.go under their tier's wrapper",
				reg, reg)
		}
	}
}
