#!/bin/bash

set -euo pipefail

# Load deploy env vars if file exists (never commit .env.deploy)
if [ -f "$(dirname "$0")/.env.deploy" ]; then
  # shellcheck disable=SC1091
  source "$(dirname "$0")/.env.deploy"
fi

DEPLOY_SERVER="${DEPLOY_SERVER:?DEPLOY_SERVER is required (e.g. root@1.2.3.4)}"
DEPLOY_SSH_KEY="${DEPLOY_SSH_KEY:-$HOME/.ssh/id_rsa}"
DEPLOY_DOMAIN="${DEPLOY_DOMAIN:?DEPLOY_DOMAIN is required (e.g. api.example.com)}"

SSH="ssh -i $DEPLOY_SSH_KEY -o ServerAliveInterval=30 -o ConnectTimeout=15"
BIN_NAME="vartalaap"
LOCAL_BIN="./$BIN_NAME"
REMOTE_DIR="/opt/vartalaap"

echo "==> Building linux binary..."
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$LOCAL_BIN" ./cmd/server

echo "==> Uploading artifacts..."
# Substitute DEPLOY_DOMAIN into the nginx template before uploading
NGINX_TMP=$(mktemp)
DEPLOY_DOMAIN="$DEPLOY_DOMAIN" envsubst '$DEPLOY_DOMAIN' < deploy/nginx-api.conf > "$NGINX_TMP"

scp -i "$DEPLOY_SSH_KEY" -o ServerAliveInterval=30 -o ConnectTimeout=15 \
  "$LOCAL_BIN" deploy/vartalaap.service .env "$DEPLOY_SERVER:/tmp/"
scp -i "$DEPLOY_SSH_KEY" -o ServerAliveInterval=30 -o ConnectTimeout=15 \
  "$NGINX_TMP" "$DEPLOY_SERVER:/tmp/nginx-api.conf"
rm "$NGINX_TMP"

echo "==> Installing and restarting service..."
$SSH "$DEPLOY_SERVER" DEPLOY_DOMAIN="$DEPLOY_DOMAIN" bash <<'REMOTE'
  set -euo pipefail
  if ! id -u vartalaap >/dev/null 2>&1; then
    useradd -r -s /bin/false vartalaap
  fi
  install -d -o vartalaap -g vartalaap /opt/vartalaap
  install -d /etc/systemd/system
  install -d /etc/nginx/sites-available
  install -d /etc/nginx/sites-enabled
  install -m 644 /tmp/vartalaap.service /etc/systemd/system/vartalaap.service
  install -m 644 /tmp/nginx-api.conf /etc/nginx/sites-available/vartalaap-api
  ln -sf /etc/nginx/sites-available/vartalaap-api /etc/nginx/sites-enabled/vartalaap-api
  install -m 600 -o vartalaap -g vartalaap /tmp/.env /opt/vartalaap/.env
  install -m 755 -o vartalaap -g vartalaap /tmp/vartalaap /opt/vartalaap/vartalaap
  nginx -t
  systemctl daemon-reload
  systemctl enable vartalaap
  systemctl restart vartalaap
  certbot --nginx -d "$DEPLOY_DOMAIN" --non-interactive --agree-tos --register-unsafely-without-email || true
  systemctl reload nginx
  sleep 2
  systemctl is-active vartalaap
REMOTE

echo "==> Done."