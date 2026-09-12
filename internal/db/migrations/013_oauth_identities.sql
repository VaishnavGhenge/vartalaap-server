-- +goose Up
CREATE TABLE IF NOT EXISTS oauth_identities (
    provider   text        NOT NULL CHECK (provider IN ('google')),
    subject    text        NOT NULL,
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, subject),
    UNIQUE (user_id, provider)
);

-- +goose Down
DROP TABLE IF EXISTS oauth_identities;
