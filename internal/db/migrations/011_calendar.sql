-- +goose Up
-- Google Calendar connection, one row per (user, provider). Re-connecting
-- upserts onto the same row so a user who revokes and reconnects doesn't
-- accumulate dead credentials.
--
-- access_token / refresh_token are AES-256-GCM ciphertext (see
-- internal/secretbox) encoded as "v1:<base64(nonce||ct)>". They are NEVER
-- stored or logged in plaintext. CALENDAR_ENCRYPTION_KEY holds the key, so a
-- database dump on its own does not grant calendar access.
--
-- revoked_at is set when Google answers a refresh with invalid_grant — the
-- user removed our access from their Google account settings. Once set we
-- stop calling Google entirely and the dashboard shows a reconnect prompt.
-- Keeping the row (rather than deleting it) is what lets the UI say
-- "reconnect" instead of silently reverting to "never connected".
CREATE TABLE IF NOT EXISTS calendar_connections (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider       text        NOT NULL DEFAULT 'google'
                               CHECK (provider IN ('google')),
    account_email  text,
    access_token   text        NOT NULL,
    refresh_token  text        NOT NULL,
    expires_at     timestamptz,
    calendar_id    text        NOT NULL DEFAULT 'primary',
    revoked_at     timestamptz,
    last_error     text,
    last_synced_at timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, provider)
);

-- Slot generation reads this on every public /slots request, keyed by user.
-- The UNIQUE constraint above already provides the index we need.

-- One row per booking we mirrored into an external calendar. Composite PK so
-- a booking can later exist in more than one provider without a schema change.
-- ON DELETE CASCADE from bookings: if the booking row goes, the mapping is
-- meaningless (the remote event is cleaned up before deletion, not after).
CREATE TABLE IF NOT EXISTS booking_calendar_events (
    booking_id  uuid        NOT NULL REFERENCES bookings(id) ON DELETE CASCADE,
    provider    text        NOT NULL DEFAULT 'google',
    event_id    text        NOT NULL,
    calendar_id text        NOT NULL DEFAULT 'primary',
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (booking_id, provider)
);

-- +goose Down
DROP TABLE IF EXISTS booking_calendar_events;
DROP TABLE IF EXISTS calendar_connections;
