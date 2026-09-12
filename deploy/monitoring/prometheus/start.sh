#!/bin/sh
set -eu
if [ "$(id -u)" = 0 ]; then
  chown 65534:65534 /prometheus
  exec su -s /bin/sh nobody -c 'exec /bin/sh /start.sh'
fi
: "${OPS_TOKEN:?OPS_TOKEN must be set}"
umask 077
printf '%s' "$OPS_TOKEN" > /tmp/ops-token
# Prometheus does not expand env vars in its config, so the environment label
# is rendered here from the Railway environment the container is running in.
sed "s/__ENVIRONMENT__/${RAILWAY_ENVIRONMENT_NAME:-unknown}/g" \
  /etc/prometheus/prometheus.yml > /tmp/prometheus.yml
exec /bin/prometheus --config.file=/tmp/prometheus.yml --storage.tsdb.path=/prometheus --storage.tsdb.retention.time=14d --storage.tsdb.retention.size=1GB --web.listen-address=0.0.0.0:9090
