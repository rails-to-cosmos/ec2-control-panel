# Make the route ACL a declared property, not a remembered wrapper

> **Status: IMPLEMENTED.** Implemented 2026-07-27: `guard`/`route`/`apiRoutes` in `src/server/server.go` + `AuthConfig.wrap` in `src/server/auth.go`, guarded by `src/server/routes_test.go`. Task routes now use the `{taskID}` wildcard so `{id}` unambiguously means an instance id.

**Pattern** — every API route's access control is a hand-typed wrapper at the
registration site. Omitting it compiles, vets and tests clean, and serves the
route to every signed-in user.

## Files

- `src/server/server.go:120-141` — 17 `mux.HandleFunc` registrations; `protect(...)`
  applied by hand to 3, `auth.requireAdmin(...)` to 2, the rest bare.
- `src/server/handlers.go:49`, `:97` — the two list-filter `CanRead` loops.
- `src/server/auth.go:448-466` (`RequireInstanceAccess`), `src/server/tasks.go:16-30`
  (`taskReadable`).
- `src/server/auth_test.go`, `src/server/stream_test.go` — neither asserts any
  route's ACL (`stream_test.go` passes `auth = nil`).

CLAUDE.md ("Access control") states: *"The ACL is enforced in two places — the
list filters and `RequireInstanceAccess` on every per-instance route. Dropping
either one leaks."* That sentence is currently the only thing enforcing it.

## Proposed change

A route table whose **zero value is the closed guard**, so a forgotten guard
fails safe instead of open:

```go
// guard selects a route's access check. The zero value is the strictest one,
// so a route added without thinking about ACLs gets the per-instance check
// rather than none.
type guard int

const (
	guardInstance guard = iota // per-instance reader ACL (default)
	guardSignedIn              // any authenticated user
	guardAdmin                 // real (never impersonated) admin
)

type route struct {
	Pattern string
	Guard   guard
	Handler http.HandlerFunc
}

func apiRoutes(env *config.EnvConfig, tm *tasks.Manager, cache *ec2.Cache, auth *AuthConfig) []route

// wrap applies g. A nil AuthConfig (auth disabled) returns h unchanged.
func (a *AuthConfig) wrap(g guard, h http.HandlerFunc) http.HandlerFunc
```

Registration collapses to:

```go
for _, rt := range apiRoutes(env, tm, cache, auth) {
	mux.HandleFunc(rt.Pattern, auth.wrap(rt.Guard, rt.Handler))
}
```

Plus the test that turns the silent omission red — no AWS, no server needed:

```go
func TestPerInstanceRoutesAreGuarded(t *testing.T) {
	for _, rt := range apiRoutes(nil, nil, nil, nil) {
		if strings.Contains(rt.Pattern, "{id}") && rt.Guard == guardSignedIn {
			t.Errorf("%s: per-instance route must not be guardSignedIn", rt.Pattern)
		}
	}
}
```

Pair it with the list-filter half (see `readable-instances-helper` below, or fold
it in): `readableInstances` makes the `CanRead` loop unforgettable for a third
list endpoint.

**Keep the documented asymmetry.** `taskReadable` fails closed and
`RequireInstanceAccess` falls through when instances.json can't be read; CLAUDE.md
records that as deliberate. This proposal does not unify them.

## LOC estimate

Added ~35 (registry + wrap + test). Removed ~10 (inline wrappers). Net **+25** —
this one buys enforcement, not lines. Per new endpoint: unchanged line count, but
the ACL becomes a required field instead of an optional wrapper.

## Risk

No wire, on-disk or public-API change. Route patterns are unchanged strings.
Test baseline: adds one test. Behaviour identical if the table is transcribed
faithfully — verify by diffing the served route list before/after.

## Existing precedent

`src/tasks/manager.go:16-23` (`type Status string` + constants) is the in-repo
idiom for naming a closed set. `AuthConfig.requireAdmin` (`auth.go`) and
`RequireInstanceAccess` already have the right wrapper shape — this proposal only
moves the decision of *which* wrapper from the call site into data.
