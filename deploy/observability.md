# Staging observability

The API exposes Prometheus metrics at `/metrics` only when the request carries
the Railway `OPS_TOKEN` as a Bearer token. Without the token the endpoint is
401; if OPS_TOKEN is not configured the endpoint is 404. The existing loopback listener on `:9091`
remains available for local development.

For a small staging setup, run Prometheus and Grafana outside the API service
(a laptop, a small VM, or Grafana Cloud). Use
`deploy/prometheus.staging.yml` as the scrape configuration and mount the
Railway `OPS_TOKEN` at `/run/secrets/vartalaap-ops-token`. Never commit the
token or put it in a dashboard URL.

Start with these panels:

- `rate(vartalaap_call_attempts_total{result=~"failed|error"}[5m])`
- `histogram_quantile(0.95, sum by (le) (rate(vartalaap_time_to_first_media_seconds_bucket[5m])))`
- `vartalaap_active_peers` and `vartalaap_active_rooms`
- `rate(vartalaap_sfu_repairs_total{outcome="attempted"}[5m])`
- `rate(vartalaap_sfu_repairs_total{outcome="recovered"}[5m])`
- `rate(vartalaap_http_request_duration_seconds_count[5m])`

Do not add room IDs, participant IDs, or arbitrary browser strings as labels.
The browser already sends bounded call-setup observations through signaling;
the server is the sole owner of the Prometheus registry.

## Deployed call lab

The `sessionly-api` Railway project's **staging** environment has two separate
services: `prometheus` and `grafana`. Their reproducible build contexts are under
`deploy/monitoring/`. Prometheus scrapes the staging API every 15 seconds through
private networking using a reference to the API's OPS_TOKEN. It has no public
domain. Retention is limited to 14 days or 1 GB, whichever is reached first.
Both services have persistent volumes; this is a single-instance lab, not an HA
monitoring system. Railway bills for their compute and storage.
The entrypoints initialize ownership of the mounted data directory, then drop
to the image's unprivileged user. Railway `RAILWAY_RUN_UID=0` allows that one
initialization step. Grafana uses `/api/health` as its deployment healthcheck;
Prometheus uses `/api/v1/status/buildinfo` (the Railway MCP validator rejects
the hyphen in Prometheus's conventional `/-/ready` path).

Dashboard: https://grafana-staging-6599.up.railway.app/d/vartalaap-call-lab

Sign in as `admin`. Retrieve `GF_SECURITY_ADMIN_PASSWORD` from Railway's Grafana
service variables in staging. Signups and anonymous access are disabled.
Dashboard and datasource definitions are provisioned from files; edit the JSON
in this repository and redeploy Grafana to change panels. Grafana's official
provisioning reference: https://grafana.com/docs/grafana/latest/administration/provisioning/

Before a test call, select the last 30 minutes and confirm `API scrape healthy`
is 1. Note the call's start/end times and participant count. Use an absolute time
range afterward to review first-media latency, setup outcomes, network quality,
repair attempts/recoveries, and API health together.

RTT/loss/jitter histograms are stream-report samples from the existing browser
stats message. Larger rooms and longer calls contribute more samples. They do
not represent per-call percentiles. Missing RTT (-1) is excluded. Video-held
samples represent intentional adaptive quality, not video freezes. Actual
rendered-frame freezes and unexpected-disconnect SLOs still need dedicated
instrumentation; do not infer those from repair counters. No room IDs, names,
or participant IDs are exported as Prometheus labels.

No data is not a successful call. With no attempts, success percentage is
undefined. At this scale inspect outcome counts alongside percentages. The slow
setup alert requires at least five observations; one failed setup raises a
warning. Prometheus evaluates five alert rules, visible through the dashboard's
pending/firing panel. External email/chat notification delivery is not configured.

## Deploy and validate

Deploy the `prometheus` and `grafana` subdirectories separately with Railway
`up --path-as-root`, targeting their staging service IDs explicitly. The API is
deployed from the server repository root. Never deploy a monitoring subdirectory
to the API service. Preserve the volumes and Grafana admin secret on redeploys.

Validate Prometheus before deploying:

```sh
docker build -t vartalaap-prometheus-check deploy/monitoring/prometheus
docker run --rm --entrypoint /bin/promtool vartalaap-prometheus-check check config --syntax-only /etc/prometheus/prometheus.yml
```

After deploy, verify the exact deployment reached SUCCESS, Grafana's datasource
health endpoint succeeds, and `up{job="vartalaap-api"}` returns 1. Run the staging
call tests and confirm the media histogram sample counts increase. A green
Railway deployment alone does not verify metrics collection.

From the server repository, `node deploy/monitoring/verify.mjs --require-media`
verifies authenticated datasource access, all dashboard expressions, denied
anonymous access, scrape health, nonzero media samples, and rule evaluation.
It reads the Grafana credential through Railway CLI without printing it. Run
the client's `sfu-tracks.spec.ts -g 'periodic network-quality telemetry'` test
against staging first; its sustained call waits for two browser reports.
