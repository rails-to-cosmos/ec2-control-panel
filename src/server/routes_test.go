package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The zero value must be the strictest guard: a route literal that omits Guard
// then gets the per-instance ACL instead of no check at all.
func TestGuardZeroValueIsInstanceACL(t *testing.T) {
	var g guard
	if g != guardInstance {
		t.Fatal("zero guard must be guardInstance so a forgotten guard fails closed")
	}
}

// An "{id}" wildcard is an instances.json session id (task routes use
// "{taskID}"), so such a route must be checked per instance or restricted to
// admins. Without this, a new per-instance route that forgets its guard serves
// every instance to every signed-in user, and nothing else catches it: it
// compiles, vets and passes the rest of the suite.
func TestPerInstanceRoutesAreGuarded(t *testing.T) {
	routes := apiRoutes(nil, nil, nil, nil)
	if len(routes) == 0 {
		t.Fatal("apiRoutes returned nothing")
	}
	seen := map[string]bool{}
	instanceScoped := 0
	for _, rt := range routes {
		if seen[rt.Pattern] {
			t.Errorf("%s: duplicate route pattern", rt.Pattern)
		}
		seen[rt.Pattern] = true
		if rt.Handler == nil {
			t.Errorf("%s: nil handler", rt.Pattern)
		}
		if !strings.Contains(rt.Pattern, "{id}") {
			continue
		}
		instanceScoped++
		if rt.Guard == guardSignedIn {
			t.Errorf("%s: per-instance route must not be guardSignedIn", rt.Pattern)
		}
	}
	if instanceScoped == 0 {
		t.Fatal("no {id} routes found — the convention this test relies on has changed")
	}
}

// wrap must be nil-safe: with auth disabled every route runs unguarded, which is
// how local dev and the CLI-only path work.
func TestWrapNilAuthPassesThrough(t *testing.T) {
	var a *AuthConfig
	called := false
	h := a.wrap(guardInstance, func(w http.ResponseWriter, r *http.Request) { called = true })
	h(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Fatal("nil AuthConfig must pass the handler through unwrapped")
	}
}
