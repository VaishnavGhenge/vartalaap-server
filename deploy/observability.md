# Staging observability

The API exposes Prometheus metrics at `/metrics` only when the request carries
the Railway `OPS_TOKEN` as a Bearer token. Without the token the endpoint is
404; with a wrong token it is 401. The existing loopback listener on `:9091`
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
