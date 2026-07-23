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
- `InstanceConfig.CanRead`: **closed by default** — admins bypass; an empty
  `readers` list means *admins only*; `"*"` (`config.ReadersPublic`) means any
  signed-in user; otherwise membership decides. Adding an instance without
  `readers` hides it from everyone but admins, on purpose.
- The ACL is enforced in *two* places — the list filters (`handleInstances`,
  `handleStatuses`) and `RequireInstanceAccess` on every per-instance route.
  Dropping either one leaks. Resolve identity via `AuthConfig.reader(r)`, which
  is nil-safe (auth disabled ⇒ admin).
- Task endpoints inherit the instance ACL via `taskReadable` (list, get and
  stream alike), so operation logs never leak across instances.
- Asymmetry to know about: `taskReadable` fails **closed** when instances.json
  can't be read or the id is gone, while `RequireInstanceAccess` falls through
  to the handler in those cases (fails open).
- Admin-only writes (`PATCH /api/instances/{id}`, `POST /api/users`,
  `POST /api/view-as`) gate on the **real** session identity via
  `requireAdmin`, never `reader()` — impersonation must not widen access, and an
  admin viewing-as a non-admin still has to be able to clear the cookie.
- `POST /api/instances` is deliberately NOT admin-gated: any signed-in user may
  add an instance.
- The `view-as` cookie is plaintext and unsigned; it is inert because `reader()`
  only consults it when the real user is an admin. Removing that precondition
  would turn a cookie into full impersonation.
- Usernames are the **lowercased** Google email local-part, but `readers`,
  `EC2CP_ADMINS` and `OAUTH_ALLOWED_USERS` are not lowercased when loaded —
  casing must match exactly or access silently fails.

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
