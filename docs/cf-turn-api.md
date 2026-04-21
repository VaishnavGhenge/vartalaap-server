# Cloudflare TURN Credentials API — pinned shape

Source: https://developers.cloudflare.com/calls/turn/generate-credentials/
Captured: 2026-04-22

## Generate short-lived ICE server credentials

- **Method:** `POST`
- **URL:** `https://rtc.live.cloudflare.com/v1/turn/keys/{TURN_KEY_ID}/credentials/generate-ice-servers`
- **Auth header:** `Authorization: Bearer {TURN_KEY_API_TOKEN}`
- **Content-Type:** `application/json`
- **Success status:** `201 Created`

### Request body

```json
{
  "ttl": 86400
}
```

- `ttl` — credential lifetime in seconds.

### Response body

**Note:** `iceServers` is an **array** of server objects (one for STUN, one for TURN). Not a single object.

```json
{
  "iceServers": [
    {
      "urls": [
        "stun:stun.cloudflare.com:3478",
        "stun:stun.cloudflare.com:53"
      ]
    },
    {
      "urls": [
        "turn:turn.cloudflare.com:3478?transport=udp",
        "turn:turn.cloudflare.com:53?transport=udp",
        "turn:turn.cloudflare.com:3478?transport=tcp",
        "turn:turn.cloudflare.com:80?transport=tcp",
        "turns:turn.cloudflare.com:5349?transport=tcp",
        "turns:turn.cloudflare.com:443?transport=tcp"
      ],
      "username": "<username>",
      "credential": "<credential>"
    }
  ]
}
```

## Implementation notes for `internal/cfturn/client.go`

1. Endpoint path is `/credentials/generate-ice-servers` (NOT `/credentials/generate` as the plan sketch had).
2. Response `iceServers` is `[]IceServer`, not a single `IceServer`. Struct must be:
   ```go
   type CredentialsResponse struct {
       IceServers []IceServer `json:"iceServers"`
   }
   ```
   `IceServer` stays as `{ URLs []string, Username, Credential string }`.
3. Port 53 URLs are known-blocked in browsers. Our `/ice-servers` handler can pass them through (browser will skip), or we can filter them server-side. Leave pass-through for v1.
4. `201 Created` is the success status — our client's `resp.StatusCode >= 400` check already handles this correctly (201 < 400).

## Example curl

```bash
curl -X POST \
  -H "Authorization: Bearer $CF_TURN_API_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{"ttl": 3600}' \
  https://rtc.live.cloudflare.com/v1/turn/keys/$CF_TURN_KEY_ID/credentials/generate-ice-servers
```
