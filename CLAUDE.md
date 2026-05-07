# vartalaap-server — Backend Rules

Go signaling server. WebSocket relay + ICE credentials endpoint backed by Cloudflare TURN.

---

## Go patterns

- Errors are returned, not panicked. Only panic on programmer errors (nil dereference of something that can never be nil).
- Every goroutine must have a defined owner and a defined exit condition. No fire-and-forget goroutines without a `context.Context` cancel or a `done` channel.
- Timeouts on every external call: HTTP clients, WebSocket reads, database queries. The default Go client has no timeout — always set one explicitly.

## Signaling server rules

- Hub, Room, and Client are the three core types. New features extend these — don't create parallel structures.
- A client disconnect must always clean up: remove from room, notify peers, release any held resources. Verify this path in every change that touches connection lifecycle.
- Messages are defined in `internal/signaling/message.go`. New message types go there with a comment explaining the sender and receiver.

## Failure paths to always handle

- WebSocket write failures — the remote peer may have disconnected between read and write
- Room cleanup when the last participant leaves — no orphaned room state
- TURN credential fetch failure — return a usable error to the client, don't block the call
- Goroutine leaks on context cancellation — use `select` with `ctx.Done()` in every loop
