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
./run.sh                        # docker compose down/up rebuild
```

The Docker image is multi-stage (`golang:1.24-bookworm` → `alpine:3.20`),
runs `ec2cp serve` on port 2720 with host networking.

## Layout

```
cmd/ec2cp/        # binary entry point
internal/
  cli/            # cobra subcommands
  ec2/            # business logic + AWS SDK calls + cloud-init templates
  config/         # env + instances.json + dotenv loader
  progress/       # context-bound logf writer
  server/         # HTTP server + handlers + embedded UI
  tasks/          # async task queue (HTTP only)
Dockerfile        # multi-stage build (golang → alpine)
docker-compose.yml
```

## Safety

- `stop` and `restart` prompt for confirmation unless `--yes` is passed or the
  session id is `test`.
- Spot launches use `--bid-price` (or `EC2_SPOT_BID_PRICE`); failed
  fulfillments surface the AWS reason (`price-too-low`,
  `capacity-not-available`, ...) and are auto-cancelled.
