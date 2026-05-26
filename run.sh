set -a
source .env
# .env.local overrides — copy .env.local.example to .env.local for local dev
# (sets SECURE_COOKIE=false and routes email through Mailpit instead of Resend).
[ -f .env.local ] && source .env.local
set +a
go run ./cmd/server
