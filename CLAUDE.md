# ec2cp — agent notes

Go CLI + HTTP server managing per-user EC2 sandboxes. `cmd/ec2cp` is the entry
point; business logic lives in `src/ec2`, config in `src/config`, the HTTP API +
embedded UI in `src/server`. Build/test: `go build ./...`, `go vet ./...`,
`go test ./...`.

## Invariants

Rules the codebase enforces silently. Changing any of these needs deliberate care.

### instances.json
- `writeInstances` tries an atomic temp-file + rename, then **falls back to an
  in-place write**: in production instances.json is a single-file bind mount and
  renaming onto a mount point fails with `EBUSY`. Never drop the fallback.
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
- `InstanceConfig.CanRead`: admins bypass, an **empty `readers` list is public**
  to any signed-in user, otherwise membership decides.
- The ACL is enforced in *two* places — the list filters (`handleInstances`,
  `handleStatuses`) and `RequireInstanceAccess` on every per-instance route.
  Dropping either one leaks. Resolve identity via `AuthConfig.reader(r)`, which
  is nil-safe (auth disabled ⇒ admin).
- Known gap: `GET /api/tasks/{id}/stream` is **not** reader-gated, so any
  signed-in user who guesses a task id can read its output.
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

### Misc
- `.env` never overrides the process environment (`godotenv.Load`, not
  `Overload`) — container/CI vars win.
- The task manager allows at most one running task per `sessionID`, and eviction
  never drops an active task — this is what stops concurrent destructive
  start/stop against the same EBS volume.
