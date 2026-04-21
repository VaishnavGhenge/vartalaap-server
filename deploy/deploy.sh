#!/bin/bash

set -euo pipefail

SERVER="root@REDACTED_SERVER_IP"
SSH_KEY="$HOME/.ssh/REDACTED_SSH_KEY"
SSH="ssh -i $SSH_KEY -o ServerAliveInterval=30 -o ConnectTimeout=15"
BIN_NAME="vartalaap"
LOCAL_BIN="./$BIN_NAME"
REMOTE_DIR="/opt/vartalaap"

echo "==> Building linux binary..."
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$LOCAL_BIN" ./cmd/server

echo "==> Uploading artifacts..."
scp -i "$SSH_KEY" -o ServerAliveInterval=30 -o ConnectTimeout=15 \
  "$LOCAL_BIN" \
  deploy/vartalaap.service \
  deploy/nginx-api.conf \
  .env \
  "$SERVER:/tmp/"

echo "==> Installing and restarting service..."
$SSH "$SERVER" bash <<'REMOTE'
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
  systemctl enable --now vartalaap
  certbot --nginx -d REDACTED_DOMAIN --non-interactive --agree-tos --register-unsafely-without-email
  systemctl reload nginx
  sleep 2
  systemctl is-active vartalaap
REMOTE

echo "==> Done."
