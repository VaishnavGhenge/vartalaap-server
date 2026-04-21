# Deploying vartalaap-server

## First-time VM setup

```bash
sudo useradd -r -s /bin/false vartalaap
sudo mkdir -p /opt/vartalaap
sudo chown vartalaap:vartalaap /opt/vartalaap
sudo cp deploy/vartalaap.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable vartalaap
```

Create `/opt/vartalaap/.env`:

```env
CF_TURN_KEY_ID=...
CF_TURN_API_TOKEN=...
PORT=8080
ALLOWED_ORIGINS=https://<your-client-domain>
```

## Deploying from local machine

```bash
./deploy/deploy.sh
```

## Verify on VM

```bash
sudo systemctl status vartalaap
sudo journalctl -u vartalaap -n 100 --no-pager
```

## Reverse proxy

Point your reverse proxy at `localhost:8080`.
