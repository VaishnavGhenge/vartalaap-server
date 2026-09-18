# vartalaap-server

Go signaling server for Vartalaap. Provides WebSocket signaling relay and an ICE-credentials endpoint backed by Cloudflare TURN.

Google sign-in uses the same OAuth client ID and secret as Calendar sync, but a separate callback and identity-only scopes. Configure:

- `GOOGLE_CLIENT_ID`
- `GOOGLE_CLIENT_SECRET`
- `GOOGLE_AUTH_REDIRECT_URL` — for production: `https://api.getsessionly.com/auth/google/callback`
- `PUBLIC_APP_URL` — for production: `https://www.getsessionly.com`

Register `GOOGLE_AUTH_REDIRECT_URL` as an authorized redirect URI on the Google OAuth web client. Calendar access remains a separate consent flow configured by `GOOGLE_REDIRECT_URL`.

## CI/CD

`.github/workflows/deploy.yml` runs `go test ./...` and `go vet ./...` for every push to `main`. After those checks pass, it uploads the repository to the production `sessionly-api` Railway service and waits for that exact deployment to reach `SUCCESS`.

Configure a GitHub Actions environment named `production` and add a `RAILWAY_TOKEN` secret containing a Railway project token with deploy access to the Sessionly production project. The workflow does not read application secrets from GitHub; runtime variables remain configured in Railway.
