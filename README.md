# vartalaap-server

Go signaling server for Vartalaap. Provides WebSocket signaling relay and an ICE-credentials endpoint backed by Cloudflare TURN.

Google sign-in uses the same OAuth client ID and secret as Calendar sync, but a separate callback and identity-only scopes. Configure:

- `GOOGLE_CLIENT_ID`
- `GOOGLE_CLIENT_SECRET`
- `GOOGLE_AUTH_REDIRECT_URL` — for production: `https://api.getsessionly.com/auth/google/callback`
- `PUBLIC_APP_URL` — for production: `https://www.getsessionly.com`

Register `GOOGLE_AUTH_REDIRECT_URL` as an authorized redirect URI on the Google OAuth web client. Calendar access remains a separate consent flow configured by `GOOGLE_REDIRECT_URL`.
