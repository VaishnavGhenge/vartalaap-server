#!/bin/bash

set -euo pipefail

SERVER="root@REDACTED_SERVER_IP"
SSH_KEY="$HOME/.ssh/REDACTED_SSH_KEY"
SSH="ssh -i $SSH_KEY -o ServerAliveInterval=30 -o ConnectTimeout=15"
REMOTE_DIR="/opt/vartalaap"
BIN_NAME="vartalaap"
LOCAL_BIN="./$BIN_NAME"

echo "==> Building linux binary..."
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$LOCAL_BIN" ./cmd/server

echo "==> Uploading binary..."
scp -i "$SSH_KEY" -o ServerAliveInterval=30 -o ConnectTimeout=15 "$LOCAL_BIN" "$SERVER:/tmp/$BIN_NAME"

echo "==> Installing and restarting service..."
$SSH "$SERVER" bash <<'REMOTE'
  set -euo pipefail
  mkdir -p /opt/vartalaap
  mv /tmp/vartalaap /opt/vartalaap/vartalaap
  chmod +x /opt/vartalaap/vartalaap
  systemctl restart vartalaap
  sleep 2
  systemctl is-active vartalaap
REMOTE

echo "==> Done."
