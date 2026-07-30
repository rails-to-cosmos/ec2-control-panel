# ec2cp — agent notes

Go CLI + HTTP server managing per-user EC2 sandboxes. `cmd/ec2cp` is the entry
point; business logic lives in `src/ec2`, config in `src/config`, the HTTP API +
embedded UI in `src/server`. Build/test: `go build ./...`, `go vet ./...`,
`go test ./...`.

## Invariants

Rules the codebase enforces silently. Changing any of these needs deliberate care.

### instances.json
- Every JSON store writes through `config.WriteFileAtomic`: temp-file + rename,
  then **falling back to an in-place write**. In production instances.json is a
  single-file bind mount and renaming onto a mount point fails with `EBUSY`.
  Never drop the fallback, and never hand-roll a second writer (the status cache
  used to, and silently lacked the fallback).
- A **manual** host-side swap must stay in place (`cat new > instances.json`,
  never `mv`) — `mv` gives the file a new inode and detaches the bind mount.
- `LoadInstances` uses `DisallowUnknownFields`, and one unknown key fails the
  decode for the **whole file**. Deploy a new `InstanceConfig` field before
  writing data that uses it.
- `AddInstance` serializes read-modify-write under `instancesMu` — in-process
  only; a CLI run or manual edit racing the server is last-writer-wins.
- `encoding/json` sorts string map keys, so the file stays stably sorted. Don't
  switch `Instances` to a slice or custom marshaler without accepting churn.

### Access control
- `InstanceConfig.CanRead`: **closed by default** — admins bypass; a non-empty
  `Owner` always reaches their own instance; an empty `readers` list means
  *admins only*; `"*"` (`config.ReadersPublic`) means any signed-in user;
  otherwise membership decides. Adding an instance without `readers` hides it
  from everyone but its owner and admins, on purpose. The owner clause is what
  makes `readers` safe to rewrite: no edit can orphan an instance from the
  person who owns it.
- The ACL is enforced in *two* places — the list filters (`handleInstances`,
  `handleStatuses`) and the route guard on every per-instance route. Dropping
  either one leaks. Resolve identity via `AuthConfig.reader(r)`, which is
  nil-safe (auth disabled ⇒ admin).
- Guards are declared, not remembered: `apiRoutes` gives every route a `Guard`,
  and `guardInstance` is the **zero value**, so a route added without thought
  gets the per-instance check. Two traps `routes_test.go` cannot catch:
  `RequireInstanceAccess` reads `r.PathValue("id")`, so a per-instance route
  whose wildcard is named anything else silently matches nothing and passes
  unguarded; and `wrap`'s `default:` returns the handler unwrapped, so a new
  guard constant without a `case` is born unenforced.
- `guardSignedIn` enforces nothing by itself — the session requirement comes
  from `auth.middleware`, applied once around the whole mux.
- Task endpoints inherit the instance ACL via `taskReadable` (list, get and
  stream alike), so operation logs never leak across instances.
- Asymmetry to know about: `taskReadable` fails **closed** when instances.json
  can't be read or the id is gone, while `RequireInstanceAccess` falls through
  to the handler in those cases (fails open).
- Admin-only writes (`POST /api/users`, `POST /api/view-as`) gate on the
  **real** session identity via `requireAdmin`, never `reader()` — impersonation
  must not widen access, and an admin viewing-as a non-admin still has to be
  able to clear the cookie. `POST /api/view-as` is registered in
  `registerAuthRoutes`, outside `apiRoutes`, so the route test never sees it.
- `PATCH /api/instances/{id}` is per-instance, not admin-only: anyone who can
  read an instance may configure it. A non-admin is force-added back to any
  non-public `readers` list so they cannot configure themselves out. The create
  path does *not* have that guard for an explicitly empty list; a bare-API
  `readers: []` from a non-admin leaves the instance visible to its owner and
  admins only, because `Owner` is set from the creator.
- `Owner` on create comes from the **effective** identity (`reader`) so
  creating while impersonating produces that user's instance; `added_by` in the
  user registry comes from the **real** one, because it is an audit trail.
- `POST /api/instances` is deliberately NOT admin-gated: any signed-in user may
  add an instance.
- The `view-as` cookie is plaintext and unsigned; it is inert because `reader()`
  only consults it when the real user is an admin. Removing that precondition
  would turn a cookie into full impersonation.
- Usernames are the **lowercased** Google email local-part, but `readers`,
  `EC2CP_ADMINS` and `OAUTH_ALLOWED_USERS` are not lowercased when loaded —
  casing must match exactly or access silently fails.
- Admin rights are the **union** of `EC2CP_ADMINS` and the registry's `admin`
  flag, which `PATCH /api/users/{username}` sets. `isAdmin` runs on every
  guarded request, so the registry half is cached and reloaded only when
  users.json changes (path+size+mtime); a read error keeps the previous
  snapshot rather than stripping everyone mid-request.
- Two refusals keep that grant from becoming a trap: revoking **your own**
  rights is a 409 (nobody may lock themselves out), and so is revoking an
  `EC2CP_ADMINS` grant — the union means it would report success and change
  nothing. `SetUserAdmin` also refuses an unknown user, so a typo cannot create
  a phantom admin.
- `RenameUser` rewrites **instances.json first**, then the registry: a username
  is the only thing tying an instance to its owner and readers, so a failure
  between the two writes must leave the access moved rather than orphaned. It
  refuses a name that already exists (that would merge two identities), and the
  handler refuses renaming yourself (your session would name a user that is
  gone) or an `EC2CP_ADMINS` member (the env still names the old one).
- `DeleteUser` is a registry removal, **not** a revocation: an OAuth sign-in
  registers the name again and instances.json may still grant it. The handler
  returns `stillReferencedBy` for exactly that reason, and deliberately leaves
  instances.json alone — clearing `Owner` would orphan a live box to
  admins-only.

### Auth / sessions
- Sessions are stateless HMAC-signed cookies. `unsign` MACs the received body
  bytes, so payload key order never matters.
- One `b64` alphabet (`base64.RawURLEncoding`) is shared by cookie signing *and*
  PBKDF2 password encoding — changing it invalidates every live session **and**
  every stored `EC2CP_USERS` hash. `exp` must stay a JSON number.
- Base-path duality: nginx strips the `/ec2` prefix, so routes and the
  middleware's public-path checks use **unprefixed** paths, while every emitted
  redirect/link and the session cookie `Path` use `EC2CP_BASE_PATH`.

### Storage: JSON on EFS, deliberately not SQLite
- State lives in JSON files on an EFS (nfs4) mount, written atomically. This was
  chosen over SQLite on purpose: SQLite warns that file locking is unreliable on
  network filesystems (corruption risk), WAL is unavailable there, and this
  deployment has two would-be writers — the CLI runs alongside the server, and
  `docker compose up -d` can briefly overlap old and new containers. A lost
  update is recoverable; a corrupt database is not.
- If a database is ever wanted, keep the file on local disk and snapshot it to
  EFS — never put the database itself on the network mount.

### Users
- Sign-ins (OAuth and password) upsert `EC2CP_USER_DB` (default
  `state/users.json`); admins can pre-register users. It lives in the state
  directory for the same persistence reason as the status cache.
- `RecordUser` never lets a manual entry outrank a real sign-in, and a corrupt
  registry is treated as empty rather than blocking login.

### Launch sizing
- Two distinct volumes: `LaunchParams.VolumeSize` (`EC2_INSTANCE_VOLUME_SIZE`)
  is the instance's ephemeral **root** disk, recreated every start;
  `LaunchParams.PersistentVolumeSize` (instances.json `volume_size`, else
  `EC2_VOLUME_SIZE`) sizes the **persistent EBS data volume** and is consulted
  only inside `makePersistentVolume`, i.e. once per session at first launch.
  Don't cross-wire them.

### Task streaming
- A silent task stream is dropped by idle proxies, so the server writes a NUL
  byte after `streamHeartbeat` of silence and the UI strips NULs. It is a
  two-sided contract with no shared constant: change one side alone and you get
  either garbage in the log pane or the old "Error in input stream". The
  keepalive goes to the `ResponseWriter` only — `handleTaskGet`'s `output` stays
  clean — so any non-browser consumer of the stream must strip NULs too.
- `streamHeartbeat` is a `var` so the test can shrink it; making it `const`
  breaks the only test that pins the keepalive.

### Stopping and orphan cleanup
- `Stop` unions three independent discovery handles: the volume attachment, the
  `--force` Name-tag lookup, and the session ENI's current attachment. The ENI
  path is the only one that finds a spot instance whose launch died before the
  post-fulfilment `CreateTags`, because such an instance carries no Name tag.
  Without it the next start fails with `InvalidNetworkInterface.InUse`.
- Launch tagging is asymmetric by AWS constraint: `RunInstances` tags instance
  and volume inline via `TagSpecifications`; `RequestSpotInstances` can only tag
  the *request*, so a spot instance is untagged until after fulfilment. Moving
  the on-demand tags to a post-launch `CreateTags` would recreate that orphan
  window on the safe path.
- `getSpotRequestID` prefers `Instance.SpotInstanceRequestId` over our own tag
  and returns `("", nil)` when neither exists — a missing id is not an error, or
  Stop cannot clean up the orphans it exists for.
- Every stop-path lookup is AZ-scoped, including `openSpotRequests`: two
  instances.json entries may share one AWS Name in different zones, so a
  name-only filter cancels the other session's in-flight spot request.

### Cost metering (`/metrics`)
- The meter charges running instances on the **status-poll tick**, reading the
  snapshots the poller already produced and the memoized `pricesFor` lookup, so
  it costs no extra AWS calls. Metering off a separate ticker would double the
  pricing traffic.
- Counters persist to `state/cost-meter.json` and reload on start: a redeploy
  must not look like a Prometheus counter reset. A gap longer than **4 poll
  intervals** bills nothing — the process was down, not the instance.
- `/metrics` is registered outside `apiRoutes` and is a public path in
  `auth.middleware`, so its access check lives entirely in `metricsAllowed`:
  bearer token when `EC2CP_METRICS_TOKEN` is set, otherwise loopback-only *and*
  no `X-Forwarded-For`/`X-Real-IP`. nginx proxies from loopback too, so dropping
  the forwarded-header check publishes every user's spend on `/ec2/metrics`.
- `escapeLabel` output is inserted into the exposition with plain quotes, never
  `%q` — that would escape a second time and emit `\"` inside label values.
- The instances.json id is exposed as `sandbox`. Naming it `instance` collides
  with the label Prometheus attaches for the scrape target: ours is silently
  renamed to `exported_instance` and every `by (instance)` query collapses into
  one series. Same trap for `job`.
- Prometheus's TSDB is a **local docker volume**, never the NFS mount (it mmaps
  its blocks); `prom-backup` snapshots it to EFS daily instead. Same reasoning
  as the JSON-not-SQLite decision below.
- `/grafana/*` is a reverse proxy to Grafana that authenticates *for* it via
  `auth.proxy`, so four things are load-bearing: the route is wrapped in
  `guardAdmin` (registered by hand in `Run`, invisible to `routes_test`); the
  Director **deletes** `X-WEBAUTH-USER`/`X-WEBAUTH-ROLE` before setting them,
  or any signed-in user could name themselves an admin; the identity comes from
  `UserFromContext` (the real user), never `reader()`; and nginx must route
  `/ec2/grafana/` to **ec2cp**, never to Grafana's port, which would serve the
  dashboards unauthenticated. Grafana on loopback trusts loopback — same
  boundary as `metricsAllowed`.
- The proxy re-adds `EC2CP_BASE_PATH` to the path because Grafana runs with
  `serve_from_sub_path`; nginx strips it on the way in.

### Status cache
- The poller mirrors snapshots to `EC2CP_STATE_FILE` (default `state/status-cache.json`)
  and reloads them at startup, so a restart serves the last known state instead
  of an empty table. In prod that path must live on a **mounted directory**
  (`./state:/app/state`) — a single-file mount would break the temp+rename write
  and wouldn't survive container recreation.
- `GetSubnetID` is memoized per `(vpc, az)`: it is identical for every instance
  in a zone, so without the memo each poll repeats it once per instance.

### Misc
- `.env` never overrides the process environment (`godotenv.Load`, not
  `Overload`) — container/CI vars win.
- The task manager allows at most one running task per `sessionID`, and eviction
  never drops an active task — this is what stops concurrent destructive
  start/stop against the same EBS volume.
