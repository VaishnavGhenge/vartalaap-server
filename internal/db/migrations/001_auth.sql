-- +goose Up
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text        UNIQUE NOT NULL,
    name          text        NOT NULL,
    slug          text        UNIQUE NOT NULL,
    avatar_url    text,
    password_hash text        NOT NULL,
    created_at    timestamptz DEFAULT now()
);

CREATE TABLE refresh_tokens (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  text        UNIQUE NOT NULL,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz DEFAULT now()
);

CREATE INDEX refresh_tokens_user_id_idx   ON refresh_tokens(user_id);
CREATE INDEX refresh_tokens_expires_at_idx ON refresh_tokens(expires_at);

-- +goose Down
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS users;
