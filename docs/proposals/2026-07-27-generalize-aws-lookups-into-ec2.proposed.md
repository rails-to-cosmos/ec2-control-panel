# Move the AWS lookups out of the HTTP layer

**Pattern** — `src/server/handlers.go` is the only file outside `src/ec2` that
imports the AWS SDK, and it holds ~190 lines of AWS domain logic plus three
process-lifetime caches. The package doc claims the opposite.

## Files

- `src/server/handlers.go:365-387` (`describeInstanceTypes`), `:493-541`
  (`fetchInstanceTypeSpecs`), `:543-547` (`buildEntry`)
- `:399-416` (`describeAZs`)
- `:442-467` (`describePrices`) — the spot half of a price pair whose on-demand
  half already lives in `src/ec2/pricing.go:23`
- caches: `:329` (`instanceTypesCache`), `:390` (`azCache`), `:431` (`priceCache`)
- `:340` — `instanceTypesBatchSize = 100 // AWS DescribeInstanceTypes hard limit`,
  an AWS API limit as an HTTP-package constant
- `:333-338` — `InstanceTypeEntry`, a hardware-spec type declared in `server`
  while its own field type `ec2.GpuSpec` lives in `src/ec2/aws.go:159`
- `src/server/server.go:1-3` — package doc: *"It uses the same business logic the
  CLI does (src/ec2)"*

## Proposed change

```go
// src/ec2 — InstanceTypeEntry moves here, beside GpuSpec and SpecsOf
func InstanceTypesForAZ(ctx context.Context, region, az string) ([]InstanceTypeEntry, error)
func AvailabilityZones(ctx context.Context, region string) ([]string, error)

// src/ec2/pricing.go — joins the spot half to the existing OnDemandPrice
type Price struct{ InstanceType, AZ, Spot, OnDemand string }
func Prices(ctx context.Context, region, instanceType, az string) (Price, error)
```

Handlers keep query-param parsing, env defaults and `writeJSON`. `warmCaches`
(`src/server/server.go:57-85`) calls the same three entry points, unchanged in
shape.

Two things fall out of the move:

1. **God-parameter.** `instanceTypesForAZ`, `availabilityZones`, `pricesFor`,
   `ec2.Stop` (`src/ec2/stop.go:20`) and `ec2.IP` (`src/ec2/ip.go:16`) each take
   `*config.EnvConfig` and read only `env.Region`. The new signatures take
   `region string`, matching `ec2.NewClient` and `ec2.OnDemandPrice`, which
   already do.
2. **`map[string]any` as a cache value type.** `pricesFor` returns
   `map[string]any` and *that map* is what `priceCache` stores. `Price` replaces
   it. (`src/server/handlers.go:37-45`, `:74-87`, `:135-140` already define three
   typed response structs in the same file — the correct idiom is 300 lines away.)

A sibling finding, cheap to include: `warmCaches` hand-lists three of the four
caches as bespoke goroutines. A new lookup that forgets its line there is
silently slower on first paint, with no error and no test. A
`var warmable []func(context.Context, string)` that each lookup appends to at
declaration makes `warmCaches` iterate instead of enumerate.

## LOC estimate

Added ~15 (new exported entry points, `Price`). Removed ~10 net from `server`
(the bulk is moved, not deleted). Net roughly **+5 lines, −190 lines of AWS
knowledge in the HTTP layer**. Per new AWS-backed lookup: one `ec2` function +
one thin handler, instead of a describe + memo + handler + a `warmCaches` line
in the HTTP package.

## Risk

Package-boundary move — mechanical but wide. No wire format change if the JSON
field names on `Price` match today's `map` keys (`type`, `az`, `spot`,
`onDemand`); the UI reads `spot`/`onDemand` in `paintPrice`. No on-disk change.
Verify `/api/price`, `/api/instance-types`, `/api/azs` responses byte-for-byte
before/after.

## Existing precedent

`src/ec2/aws.go:45-49` (`GetSubnetID`) is the identical pattern — an AWS describe
wrapped in `ec2.Memo` over a package-level `sync.Map` — already sitting in the
right package. `SpecsOf` (`aws.go:167`) is already shared, and `handlers.go:545`
calls it, so half the boundary is drawn correctly.
