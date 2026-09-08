# Booking notification delivery

Booking creation and cancellation commit three PostgreSQL outbox jobs in the
same transaction: guest email, host email, and calendar sync. An enqueue failure
rolls back the booking change. Production HTTP handlers do not also send inline.
No Redis or separate worker deployment is required.

The API process owns a cancellation-aware worker. A job has a two-minute lease;
delivery is limited to 30 seconds plus a five-second acknowledgement budget.
Multiple API instances claim with `FOR UPDATE SKIP LOCKED`. Lease tokens fence
late acknowledgements. Expired leases are reclaimed after crashes.

Retries use exponential backoff with jitter (initially 15–30 seconds, capped at
30–60 minutes). Jobs are not silently discarded after an attempt limit. Missing
email configuration and revoked Google access remain pending until corrected.
Booking/channel ordering prevents an earlier create from running after a later
cancellation. Cancellation brings pending create retries forward; the worker
checks the current booking before delivering. Cancelled/ended bookings do not
receive stale confirmations.

## Duplicate handling and privacy

Email recipients have independent jobs. The rendered message and sender are
snapshotted before the first provider request so retries use identical payloads.
Resend requests include a stable idempotency key and base64 calendar attachments.
Resend retains idempotency keys for **24 hours**:
https://resend.com/docs/dashboard/emails/idempotency-keys

This is at-least-once delivery, not an unconditional exactly-once guarantee.
After a very long ambiguous outage, or with a generic SMTP provider, duplicates
are still possible. Provider acceptance is not proof of inbox delivery; bounce
and delivery webhooks are not implemented here.

Calendar event IDs are deterministic. A retry after a lost insert response does
not create a second event. Cancellation falls back to that ID when a mapping
write was lost. Calendar edits made directly in Google are not imported into
Sessionly; this does not add two-way rescheduling or multi-calendar selection.

Pending job payloads contain the booking's existing contact details and magic
link credential; treat database access as sensitive. Successful jobs clear their
payloads, leaving only delivery metadata. Logs and metric labels do not include
message bodies, credentials, or recipient addresses.

## Monitoring

- `vartalaap_outbox_pending`: unfinished jobs (including delayed retries).
- `vartalaap_outbox_oldest_seconds`: age of the oldest unfinished job.
- `vartalaap_outbox_active`: active jobs on this instance (one worker per instance).
- `vartalaap_outbox_attempts_total{channel,result}`: success and retry attempts.
- `vartalaap_outbox_attempt_seconds`: attempt latency histogram.

Use `max`, not `sum`, for backlog gauges across replicas: each reads the same
database. `VartalaapNotificationBacklog` alerts when the oldest job exceeds five
minutes for two minutes. The rule must be deployed to Prometheus separately;
alert delivery still depends on the monitoring installation's notification routing.

Prometheus runs in staging only, and `prometheus.yml` labels its single API
target `environment: staging`. This alert therefore covers staging. A production
backlog raises no alert: read `vartalaap_outbox_oldest_seconds` from the
production `/metrics` endpoint directly, or add a production Prometheus service.

## Release and verification

Migration `012_booking_outbox.sql` is additive and runs with the normal API
startup migrations. This applies to **new booking changes after rollout**; it
does not resend historical confirmations or repair pre-existing orphaned events.
Drain old API instances before calling the rollout complete. Old binaries don't
enqueue outbox jobs. Rolled out to staging and production on 2026-09-08 (UTC);
production applied 012 at 19:01 UTC. Each environment runs one API replica, so
one worker per environment.

Run `go test ./...` with Docker available. `TestDurableDeliveryRecovery` uses an
isolated PostgreSQL container and simulated providers to check retries across
worker reconstruction, concurrent claims, cancellation ordering, stale leases,
and transaction rollback when enqueuing fails.

For live acceptance, use an explicitly designated host account connected to
Google and a guest inbox controlled by the tester:

1. Book a future free session. Record its booking ID.
2. Verify both actual emails and the `.ics` attachment. Check the join link.
3. Verify exactly one event in the host's primary Google calendar with the
   expected local start/end times and meeting URL.
4. Cancel through Sessionly. Verify both cancellation emails and removal of
   that exact Google event. Confirm its room is no longer joinable.
5. In staging, interrupt a provider and restart the API with jobs pending.
   Restore it and verify the queue drains without recreating cancelled events.

The live flow requires a designated Google-connected account and controlled
recipient inboxes. Local `.env` files have email settings but no Google calendar
configuration; automated tests do not establish the production connection state.
