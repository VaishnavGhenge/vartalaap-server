-- +goose Up
CREATE TABLE booking_outbox (
    id bigserial PRIMARY KEY,
    booking_id uuid NOT NULL REFERENCES bookings(id) ON DELETE CASCADE,
    action text NOT NULL CHECK (action IN ('created', 'cancelled')),
    channel text NOT NULL CHECK (channel IN ('guest_email', 'host_email', 'calendar')),
    payload jsonb NOT NULL,
    delivery_payload jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    available_at timestamptz NOT NULL DEFAULT now(),
    lease_until timestamptz,
    lease_token uuid,
    attempts integer NOT NULL DEFAULT 0,
    completed_at timestamptz,
    last_error text,
    UNIQUE (booking_id, action, channel)
);
CREATE INDEX booking_outbox_pending_idx ON booking_outbox(available_at, id) WHERE completed_at IS NULL;

-- +goose Down
DROP TABLE booking_outbox;
