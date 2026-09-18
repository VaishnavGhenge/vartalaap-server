-- +goose Up
ALTER TABLE booking_outbox DROP CONSTRAINT booking_outbox_action_check;
ALTER TABLE booking_outbox ADD CONSTRAINT booking_outbox_action_check
    CHECK (
        action IN ('created', 'cancelled')
        OR action LIKE 'rescheduled:%'
        OR action ~ '^reminder:(24h|1h):[0-9]+$'
    );

-- +goose Down
DELETE FROM booking_outbox WHERE action LIKE 'reminder:%';
ALTER TABLE booking_outbox DROP CONSTRAINT booking_outbox_action_check;
ALTER TABLE booking_outbox ADD CONSTRAINT booking_outbox_action_check
    CHECK (action IN ('created', 'cancelled') OR action LIKE 'rescheduled:%');
