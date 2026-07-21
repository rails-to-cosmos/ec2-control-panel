# ec2-control-panel

Per-user EC2 sandbox manager. Each user gets a session id keyed to a persistent
EBS volume; the instance backing that session is launched on demand (spot or
on-demand), torn down when stopped, and re-launched with the same volume on
restart.

## Two ways to use it

**CLI** (`ec2cp <subcommand>`):

```bash
ec2cp status <session-id>
ec2cp start  <session-id> [--instance-type=...] [--request-type=spot|ondemand] [--bid-price=...] [-a <az>]
ec2cp stop   <session-id> [--force]   # --force recovers orphaned instances
ec2cp restart <session-id>
ec2cp ip     <session-id>
ec2cp mount  <volume-name> <session-id>
```

**Web UI** (`ec2cp serve --port 2720`):
Single-page admin console. Pick an instance, see status, override
type/AZ/bid-price, and start/stop/restart with live progress streaming.

## Configuration

- `.env` — infrastructure-wide defaults (`EC2_REGION`, `EC2_AMI_ID`,
  `EC2_VPC_ID`, `EC2_SECURITY_GROUP`, etc.) plus AWS credentials.
- `instances.json` — per-session list with optional overrides
  (`availability_zone`, `instance_type`, `volume_size`, `request_type`).

Resolution priority for overridable values: CLI flag → `instances.json` →
`.env`. The CLI's `start`/`restart` reports show the source of every value.

## Build / deploy

```bash
go build -o ec2cp ./cmd/ec2cp   # local CLI build (Go 1.24+)
make docker-up                  # local: docker compose up -d --build
```

The Docker image is multi-stage (`golang:1.24-bookworm` → `alpine:3.20`),
runs `ec2cp serve` on port 2720 with host networking.

### Production (10.17.5.9)

GitLab CI (`.gitlab-ci.yml`) has two manual jobs:

- `build_image` — Kaniko build → `harbor.alberblanc.io/alberblanc/mlnn/ec2cp:app_latest`.
- `deploy` — scp `docker-compose.prod.yml` + `.env` to `~/nfs/ec2-control-panel`
  on `10.17.5.9`, then `docker compose pull && up -d`.

Run `build_image` first, then `deploy` (both `when: manual`, branch `master`).
`docker-compose.prod.yml` pulls the Harbor image and bind-mounts
`./instances.json` so UI-added instances persist across redeploys (the NFS dir
is the source of truth). `.env` is never committed — it's delivered from the
`EC2CP_ENV` CI file variable.

Required CI/CD variables: `HARBOR_TOKEN`, `EC2_SSH_KEY`, `EC2_SSH_CONFIG`
(group-level), `EC2CP_ENV` (File type — the `.env` contents). The host must
already be logged in to `harbor.alberblanc.io`.

## Auth (GitLab OAuth + password)

The web UI runs unauthenticated unless a sign-in method is configured; then
every path is gated behind a signed-cookie session (`src/server/auth.go`).
Set these in `.env` / the `EC2CP_ENV` CI variable:

| Var                                         | Purpose                                                                                            |
|---------------------------------------------|----------------------------------------------------------------------------------------------------|
| `GITLAB_URL`                                | self-hosted GitLab base, e.g. `https://gitlab.alberblanc.com`                                      |
| `GITLAB_CLIENT_ID` / `GITLAB_CLIENT_SECRET` | from a GitLab OAuth application (scope `read_user`)                                                |
| `OAUTH_CALLBACK_URL`                        | must equal the app's registered redirect URI, e.g. `https://apps.alberblanc.io/ec2/oauth/callback` |
| `OAUTH_ALLOWED_USERS`                       | optional csv allowlist; empty = any GitLab user                                                    |
| `EC2CP_USERS`                               | optional password accounts: `user:<pbkdf2-hash>,...` (see below)                                   |
| `EC2CP_COOKIE_SECRET`                       | session-signing key; ephemeral (sessions reset on restart) if unset                                |
| `EC2CP_BASE_PATH`                           | external mount prefix — set to `/ec2` behind the apps.alberblanc.io proxy                          |

Mint a password hash: `ec2cp hash-password --username alice` (reads the
password from stdin, prints the `EC2CP_USERS` entry).

Go-live: register a GitLab OAuth app (redirect URI = `OAUTH_CALLBACK_URL`),
add the vars above to `EC2CP_ENV`, run `build_image` then `deploy`. No nginx
change is needed — the login/oauth routes ride the existing `/ec2/` location.

## Layout

```
cmd/ec2cp/        # binary entry point
src/
  cli/            # cobra subcommands
  ec2/            # business logic + AWS SDK calls + cloud-init templates
  config/         # env + instances.json + dotenv loader
  progress/       # context-bound logf writer
  server/         # HTTP server + handlers + embedded UI
  tasks/          # async task queue (HTTP only)
Dockerfile             # multi-stage build (golang → alpine)
docker-compose.yml     # local dev — builds from source
docker-compose.prod.yml # prod host — pulls Harbor image + persists instances.json
.gitlab-ci.yml         # build_image (Kaniko → Harbor) + deploy (ssh 10.17.5.9)
```

## Safety

- `stop` and `restart` prompt for confirmation unless `--yes` is passed or the
  session id is `test`.
- Spot launches use `--bid-price` (or `EC2_SPOT_BID_PRICE`); failed
  fulfillments surface the AWS reason (`price-too-low`,
  `capacity-not-available`, ...) and are auto-cancelled.
