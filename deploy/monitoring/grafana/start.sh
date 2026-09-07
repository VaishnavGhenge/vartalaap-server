#!/bin/sh
set -eu
if [ "$(id -u)" = 0 ]; then
  chown 472:0 /var/lib/grafana
  exec su -s /bin/sh grafana -c 'exec /run.sh'
fi
exec /run.sh
