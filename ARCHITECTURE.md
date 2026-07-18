# Anchor architecture

A room-actor rebuild of the anchor server, **wire-compatible** with the
previous implementation: same port, same null-terminated JSON framing, same
packet types and routing behavior.

## The two big ideas

### 1. Each room is an actor (room.go)

One goroutine per room owns *all* of that room's state — clients, teams, room
settings. Nothing else touches it. Connection readers, the admin console, and
the server's tickers submit work by posting closures to the room's event
channel:

```
reader goroutines ──┐
console ────────────┼──► room.events ──► room goroutine (owns all state)
server tickers ─────┘                        │
                                             ▼
                                   client send queues ──► one writeLoop per conn
```

Consequences:

- **No mutexes in room/team/client code.** The previous implementation had
  five locks (`Server` maps, `Room.mu`, `Team.mu`, per-`Client.mu`) with
  invariants spanning several at once. This one has exactly one, guarding only
  the server's room/client registries, and it is never held while calling into
  a room — so lock-ordering deadlocks are impossible by construction.
- **Snapshot ordering is free.** `ALL_CLIENT_STATE` broadcasts can't interleave
  because only the room goroutine emits them.
- **A full send queue is handled inline.** Previously, disconnecting a stalled
  client from inside a broadcast risked re-entering `Room.mu`; here it's a
  plain function call on the same goroutine.
- Rooms are independent, so per-room serialization costs no parallelism.

### 2. The protocol is typed and parsed once (protocol.go)

Every field the server ever reads lives in one `Envelope` struct, parsed once
per packet. The payload stays an opaque `Raw` string relayed verbatim, so the
mod can keep adding packet types without server changes. protocol.go is also
the complete list of packets the server originates.

## Other deliberate changes from the previous implementation

- **Single teardown path.** Reader EOF, write errors, full send queues, admin
  kicks, and takeovers all funnel through `Room.disconnect`, which always
  broadcasts the membership change. (Previously, only reader-EOF disconnects
  notified the room.)
- **Sender identity is server-side.** Broadcast exclusion uses the client the
  connection actually belongs to, not the `clientId` written in the packet, so
  a client can't spoof another's identity.
- **Per-event panic isolation.** A panic in one packet handler kills that event,
  not the room (and not the server).

## What stayed the same on purpose

Null-delimited JSON framing, the opaque-payload relay model, single
`package main`, in-memory-only state with `stats.json` counters, bounded
single-writer send queues per connection, and the admin console commands.

The limits carried over from `main` are load-bearing, not incidental:
`MAX_PACKET_SIZE` (8 MB) with an explicit `scanner.Buffer`, `MAX_TEAM_QUEUE`
(512, dropping oldest), and the size guard on the `REQUEST_TEAM_STATE` reply.
Without them a client that outgrows bufio's 64 KB default frame is
disconnected mid-session and reconnects in a loop.
