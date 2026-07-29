# Cost metering and dashboards

Per-user spend is metered by the app itself and stored in Prometheus; Grafana
draws it. Everything runs from the same `docker compose` stack on 10.17.5.9.

```
ec2cp (:2720)  --/metrics-->  Prometheus (:2725)  <--query--  Grafana (:2726)
    |                              |                              |
 state/cost-meter.json      docker volume                  provisioned
 (EFS, counters)            (local disk)                   dashboards
                                   |
                            prom-backup --> ./backups (EFS), daily
```

## What is metered

A `costMeter` ticks on the status-poll interval (15s by default), reads the
snapshots the poller already produced, and charges every running instance for
the time since the previous tick at its current price. Pricing comes from the
same memoized `pricesFor` lookup the UI uses, so metering adds no AWS calls
beyond the first per `(instance type, AZ)`.

Series exposed at `GET /metrics`:

| metric | type | labels | meaning |
|---|---|---|---|
| `ec2cp_instance_running` | gauge | instance, owner, type, model, az | 1 while running |
| `ec2cp_instance_hourly_usd` | gauge | instance, owner, type, model, az | current $/hour, 0 when down |
| `ec2cp_instance_cost_usd_total` | counter | instance, owner | cost accrued while metered |
| `ec2cp_instance_running_seconds_total` | counter | instance, owner | observed running seconds |
| `ec2cp_instance_sessions_total` | counter | instance, owner | stopped → running transitions |
| `ec2cp_meter_last_tick_timestamp_seconds` | gauge | — | staleness check for the meter itself |

`owner` is the instances.json owner, or `unassigned`. Per-user rollups are
recording rules in `deploy/prometheus/rules.yml` (`ec2cp:cost_usd_1d:by_owner`
and friends).

Two properties worth knowing:

- **Counters survive a deploy.** They persist to `state/cost-meter.json` beside
  the status cache and reload on start, so a redeploy is not a counter reset.
- **Downtime is not billed.** A gap longer than four poll intervals means the
  process was down, not that the instance ran unobserved, so that tick charges
  nothing. Long outages therefore *under*-report rather than invent spend.

The figures are approximations: spot prices move between ticks and AWS bills
per second from its own clock, not ours.

## Access

`/metrics` sits outside the session middleware — Prometheus cannot hold a
cookie. It gates itself instead:

- `EC2CP_METRICS_TOKEN` set → `Authorization: Bearer <token>` is required;
- unset → only direct loopback connections are served. nginx also proxies from
  loopback, but a proxied request carries `X-Forwarded-For`, which is what
  separates "Prometheus on this host" from "the public `/ec2` vhost".

Grafana keeps its own login (`GRAFANA_ADMIN_PASSWORD` in `.env`): nginx does not
authenticate `/ec2/grafana/`, and ec2cp's session means nothing to Grafana.

## nginx

Add to the `apps.alberblanc.io` server block, beside the existing `/ec2/`
location:

```nginx
# No trailing slash on proxy_pass: GF_SERVER_SERVE_FROM_SUB_PATH makes Grafana
# expect to receive the /ec2/grafana prefix, so the URI must pass through whole.
# This block is a longer prefix than /ec2/, so it wins the match.
location /ec2/grafana/ {
    proxy_pass http://127.0.0.1:2726;
    proxy_set_header Host              $host;
    proxy_set_header X-Real-IP         $remote_addr;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    absolute_redirect off;

    # Grafana's live panels use a WebSocket.
    proxy_http_version 1.1;
    proxy_set_header Upgrade    $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 600;
    proxy_buffering off;
}
location = /ec2/grafana { absolute_redirect off; return 301 /ec2/grafana/; }

# The meter is loopback-only by design; make that explicit at the edge.
location = /ec2/metrics { return 404; }
```

## Storage

The Prometheus TSDB is a **local docker volume**, never the NFS mount:
Prometheus mmaps its blocks and network filesystems corrupt them — the same
reason this project keeps its own state in JSON files rather than SQLite (see
CLAUDE.md). Durability comes from `prom-backup`, which calls the snapshot API
daily and tars the result into `./backups` on EFS, keeping the last 14.

Restore:

```sh
cd ~/nfs/ec2-control-panel
sudo docker compose stop prometheus
sudo docker run --rm -v ec2-control-panel_prometheus-data:/prometheus \
  -v "$PWD/backups:/backups" alpine:3.20 \
  sh -c 'rm -rf /prometheus/* && tar xzf /backups/prometheus-YYYYMMDD.tar.gz -C /tmp \
         && mv /tmp/*/* /prometheus/'
sudo docker compose start prometheus
```

Retention is 395 days, enough for year-over-year comparison.
