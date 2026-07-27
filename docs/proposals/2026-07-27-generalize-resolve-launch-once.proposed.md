# One launch-parameter resolver instead of three

**Pattern** — the same five-step resolve + 19-field `LaunchParams` literal is
written three times. The copies have already drifted, and one lost a validation.

## Files

- `src/cli/start.go:42-79`
- `src/cli/restart.go:42-81`
- `src/server/handlers.go:637-682` (`buildLaunchParams`)

Each runs four `ec2.ResolveSource` calls, `ResolvePersistentVolumeSize`,
`inst.AWSName(sessionID)`, the `name == "" → awsName/"default"` fallback, then a
`LaunchParams` literal that is byte-identical between the two CLI files and
differs from the server's only in indentation. The genuine variation is the
override source (cobra flag vars vs `r.URL.Query()`) and the source-label strings
(`"instance-type"` vs `"instanceType (query)"`).

## Drift already present (verified)

- **`src/cli/restart.go` has no request-type validation.** `grep -c 'invalid
  request type'` returns 1 for `cli/start.go`, 1 for `server/handlers.go`, **0**
  for `cli/restart.go`. An unrecognised `--request-type` reaches the
  `switch p.RequestType` in `src/ec2/start.go:76-81`, which has **no `default`
  case**, and the launch silently no-ops (`instanceID=""`, `err=nil`).
- `restart.go:42` resolves the AZ with `FirstNonEmpty`, then calls
  `ResolveSource` again at `:53` purely to recover the source label, discarding
  the value with `_`.
- `handlers.go:551-558` (`resolveAZForRequest`) is a fourth AZ-resolution path.

## Proposed change

In `src/ec2/launch.go`, beside the primitives it already owns:

```go
// Overrides is the highest-priority input set for a launch: CLI flags or query
// params. Empty fields fall through to instances.json, then the environment.
type Overrides struct {
	InstanceName, InstanceType, RequestType, AZ, BidPrice string
}

// SourceStyle names the override channel, for the start report's "(source)"
// labels: StyleFlag renders "--instance-type", StyleQuery "instanceType (query)".
type SourceStyle int

const (
	StyleFlag SourceStyle = iota
	StyleQuery
)

// ResolveLaunch applies the flag → instances.json → env precedence once,
// records each value's origin, validates the request type, and returns a
// complete LaunchParams.
func ResolveLaunch(sessionID string, inst *config.InstanceConfig, env *config.EnvConfig,
	o Overrides, style SourceStyle) (LaunchParams, error)
```

Call sites become one call each, e.g.:

```go
return ec2.ResolveLaunch(sessionID, inst, env, ec2.Overrides{
	InstanceName: q.Get("instanceName"), InstanceType: q.Get("instanceType"),
	RequestType:  q.Get("requestType"),  AZ: q.Get("az"), BidPrice: q.Get("bidPrice"),
}, ec2.StyleQuery)
```

Fixing the drift is then automatic: validation lives in one place, so `restart`
cannot skip it. Add the missing `default:` to `src/ec2/start.go:76` regardless of
this refactor.

Optional follow-on: `LaunchParams` carries six value/source field pairs that
always move together (`InstanceType`/`InstanceTypeSource`, …). A
`type Sourced[T any] struct{ Value T; Source string }` collapses twelve fields to
six and turns the six hand-written `Logf` lines in `src/ec2/start.go:26-34` into a
loop. Worth doing in the same pass; not worth doing alone.

## LOC estimate

Added ~48 (one resolver). Removed ~87 (33 handlers.go, 28 start.go, 26
restart.go). Net **−39**. A sixth launch knob costs 6-8 lines across three files
in two packages today; after, 2 lines in one.

## Risk

Touches the destructive launch path (start/restart drive real AWS spend), so this
is the highest-risk proposal here despite the clean LOC win. No wire or on-disk
format change; the start report's label strings are user-visible text and must be
preserved per style. Recommend landing it behind a manual `ec2cp start` +
`restart` smoke against a scratch session before deploying.

## Existing precedent

`ResolveSource` and `ResolvePersistentVolumeSize` (`src/ec2/launch.go:42`, `:51`)
already put the resolution primitives in `ec2` — the boundary was drawn one level
too low, stopping short of the composition that actually varies.
