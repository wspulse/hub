# wspulse/server — Transport Layer Internals

This document describes the internal implementation mechanisms of wspulse/server
at the **transport and infrastructure layer**. Application-level semantics
(business logic, room state, message history) are out of scope here; those
belong to the consumers of wspulse/server.

---

## Table of Contents

1. [Goroutine Model](#1-goroutine-model)
2. [Heartbeat Mechanism](#2-heartbeat-mechanism)
3. [Backpressure and Send Buffer](#3-backpressure-and-send-buffer)
4. [Connection Teardown](#4-connection-teardown)
5. [Session Resumption](#5-session-resumption)
6. [Metrics](#6-metrics)

---

## 1. Goroutine Model

Every accepted WebSocket connection spawns exactly **two goroutines**:

```
ServeHTTP
  ├─ go writePump(transport) — drains session.send channel, drives Ping heartbeat
  └─ go readPump(transport, hub) — reads inbound frames, calls OnMessage, signals hub on exit
```

Both goroutines are owned by the `session` value. Neither goroutine is
accessible to the application layer; the `Connection` interface exposes only
`Send`, `Close`, and `Done`.

### Goroutine exit and cleanup

- `readPump` owns the transport-died signal: when `ReadMessage()` returns an
  error (normal close, network drop, or read deadline exceeded) `readPump`
  sends a `transportDied` message to the hub and exits.
- `writePump` owns the TCP connection close call: it calls `transport.Close()` on
  exit to ensure the underlying socket is always released.
- `session.done` is a `chan struct{}` closed exactly once (via `closeOnce`) by
  `session.Close()`. Both goroutines select on `<-session.done` as a unified
  shutdown signal.
- The `session.send` channel is **never closed by the sender** to avoid
  send-on-closed-channel panics; it is simply abandoned once `done` is
  closed.
- The `session.send` channel is **shared across reconnects** — it persists
  for the lifetime of the session, not the lifetime of a single WebSocket
  connection.

---

## 2. Heartbeat Mechanism

Heartbeats use **RFC 6455 protocol-layer Ping / Pong control frames**,
completely separate from application JSON messages.

### Parameters

| Parameter        | Default | Valid range        | Description                                                                   |
| ---------------- | ------- | ------------------ | ----------------------------------------------------------------------------- |
| `pingPeriod`     | 10 s    | (0, 5 m]           | `writePump` ticker interval; one `PingMessage` sent every 10 s                |
| `pongWait`       | 30 s    | (pingPeriod, 10 m] | Rolling `ReadDeadline` window; reset to `now + 30 s` each time a Pong arrives |
| `writeWait`      | 10 s    | (0, 30 s]          | Per-write deadline, **including the Ping control frame itself**               |
| `maxMessageSize` | 512 B   | [1, 64 MiB]        | `readPump SetReadLimit`; exceeded size triggers immediate disconnect          |
| Send buffer      | 256     | [1, 4096]          | `session.send` channel depth (configurable via `WithSendBufferSize`)          |

Configuring non-default values:

```go
server.NewServer(connect,
    server.WithHeartbeat(30*time.Second, 90*time.Second),  // pingPeriod, pongWait
    server.WithWriteWait(15*time.Second),
    server.WithMaxMessageSize(4096),
)
```

### Operational details

1. **Ping dispatch** — `writePump` calls `SetWriteDeadline(now + writeWait)` then
   sends `websocket.PingMessage` (empty payload). If the write fails, `writePump`
   exits and triggers teardown.

2. **Pong handling** — `readPump` installs a `SetPongHandler` that fires on every
   incoming Pong:
   - `SetReadDeadline(now + pongWait)` — rolls the deadline forward; connection
     stays alive as long as at least one Pong arrives every `pongWait` period.

3. **Client-side Pong** — Standard WebSocket implementations (browsers, Gorilla)
   reply to Ping automatically; the application layer does not need to handle this.
   Note: native clients (Go, Node.js) **also** send their own independent Ping to
   the server for client-side dead-connection detection. The server auto-replies
   to these via gorilla's default `PingHandler`. See the protocol spec for details.

4. **Timeout disconnect** — If no Pong arrives within `pongWait`, `ReadMessage()`
   returns an `i/o timeout` error. `readPump` exits, unregisters the connection
   from the hub, and the `OnDisconnect` callback fires.

---

## 3. Backpressure and Send Buffer

Each session maintains a `session.send chan []byte` with a configurable
depth (default **256**). When `Server.Broadcast` or `Server.Send` is called:

1. The encoded frame bytes are sent to `session.send` via a non-blocking select.
2. If the channel is **full**, `ErrSendBufferFull` is returned to the caller
   (for direct `Send`) or **drop-oldest** backpressure is applied (for
   `Broadcast`): the oldest frame in the connection's send buffer is discarded
   to make room for the new frame; if the buffer is still full after that, the
   new frame is silently dropped.
3. When `resumeWindow > 0` and the session is suspended (no active WebSocket),
   frames are buffered to an in-memory `ringBuffer` instead of the send channel.
   These frames are replayed when the client reconnects.

This ensures a slow or lagging connection cannot block the hub event loop or
stall broadcasts to other healthy connections.

If the application needs reliable delivery for a specific connection, it should
call `connection.Send` directly and handle `ErrSendBufferFull` at the call
site — for example, by scheduling a retry or incrementing a dropped-frames
metric.

---

## 4. Connection Teardown

Normal and abnormal teardown follow the same cleanup path:

```
cause (close frame / network drop / ReadDeadline)
  → readPump: ReadMessage() returns error, sends transportDiedMessage to hub
  → hub: if resumeWindow > 0 → suspend session (start grace timer)
         if resumeWindow == 0 → remove session, call OnDisconnect
  → session.Close() (via closeOnce): closes done channel
  → writePump: selects <-pumpQuit or <-session.done, stops ticker,
    calls transport.Close()
```

Explicit teardown via `Server.Kick(connectionID)` takes a different path — the
kick request is routed through the hub to ensure serialized cleanup (see
§5 "Kick Bypass" for details).

The `closeOnce sync.Once` guard ensures `close(done)` executes exactly once
regardless of which side (readPump, writePump, grace timer expiry, or an
explicit `Server.Kick` call) initiates the teardown.

---

## 5. Session Resumption

When `WithResumeWindow(d)` is configured with `d > 0`, wspulse/server
introduces a **session layer** that decouples the application-visible
`Connection` from the underlying WebSocket transport. This allows transparent
reconnection without leaking connect/disconnect events to the application
layer.

> **Type:** `WithResumeWindow` accepts a `time.Duration`.
> `WithResumeWindow(30 * time.Second)` means a 30-second grace window.
> Valid range: 0 (disabled) … no upper limit.

### Architecture

`Connection` (public interface) is implemented by `session` (private struct).
The session holds a `*websocket.Conn` (`transport`) representing the current
physical connection. When the WebSocket dies, the session enters a
**suspended** state and starts a grace timer. If the client reconnects with
the same `connectionID` before the timer expires, the new WebSocket is
swapped in silently.

### Session State Machine

```
[*] → Connected : handleRegister creates session + transport

Connected → Suspended : transport dies, resumeWindow > 0 (start timer, buffer frames, onTransportDrop fires)
Connected → Closed    : transport dies, resumeWindow == 0 (onDisconnect fires)
Connected → Closed    : Kick() or Close() (onDisconnect fires)
Connected → Closed    : duplicate connectionID arrives (old session kicked, onDisconnect fires with ErrDuplicateConnectionID; new session created)

Suspended → Connected : same connectionID reconnect (cancel timer, replay buffer, onTransportRestore fires)
Suspended → Closed    : timer expires (onDisconnect fires, session destroyed)
Suspended → Closed    : Connection.Close() called (cancel timer, onDisconnect fires immediately)
Suspended → Closed    : Kick() or server.Close() (cancel timer, onDisconnect fires immediately)

Closed → [*]
```

### WebSocket Swap Sequence

```
WS1 connection drops
  → WS1 sends transportDiedMessage(session, err) to Hub
  → Hub detaches WS1, starts resumeWindow timer
  → go onTransportDrop(session, err)
  → Session state = suspended, frames buffered to ringBuffer

WS2 client reconnects with same connectionID
  → WS2 sends register(connectionID, transport) to Hub
  → Hub cancels timer
  → Hub attaches WS2, drains ringBuffer to send channel
  → Hub starts writePump(WS2) + readPump(WS2)
  → onTransportRestore fires; no onConnect / onDisconnect fired
```

Note: on resume, the `roomID` returned by the new `ConnectFunc` call is
ignored. The session retains its original room assignment from the initial
connection.

### Resume Buffer

During the suspended state, frames sent via `session.Send()` or
`Server.Broadcast()` are stored in an in-memory `ringBuffer` with a capacity
equal to `sendBufferSize` (default 256 frames). When the buffer is full, the
oldest frame is dropped (same backpressure strategy as the send channel during
normal operation).

On reconnect, buffered frames are drained from the ring buffer into the send
channel before the new `writePump` starts, ensuring ordering is preserved.

### Effective Reconnect Window

The total time a client has to reconnect is:

```
effective window = pongWait + resumeWindow
```

- `pongWait` (default 30 s): time before the server detects the dead transport.
- `resumeWindow` (configured via `WithResumeWindow`): additional grace period
  after detection.

The client's exponential backoff reconnect (1 s, 2 s, 4 s, 8 s, ...) can
attempt multiple retries within this window.

### Kick Bypass

`Server.Kick(connectionID)` always destroys the session immediately, bypassing
the resume window. Kick is an explicit application-layer action that signals
intentional removal, not a transient network failure.

#### Hub-routed Kick

Kick is routed through the hub's event loop via a `kickRequest` channel.
This ensures that map removal, `session.Close()`, and the `OnDisconnect`
callback are all serialized with other state mutations (register,
transportDied, graceExpired). Without this, calling `Close()` directly on a
**suspended** session would close `session.done` but leave the session in hub
maps until the grace timer fires.

To prevent `Kick` from double-firing `onDisconnect` when a grace timer happens
to fire simultaneously, `removeSession` (called by `handleKick`) bumps
`suspendEpoch`. The stale grace message is then discarded by
`handleGraceExpired`'s epoch check.

```
Kick(connectionID) → kickRequest{connectionID, result} → hub.kick channel
hub.run() selects kickRequest
  → handleKick: removeSession + session.Close() + go onDisconnect()
  → writes nil to result channel
Kick() returns nil
```

If the hub has already shut down (`<-hub.done`), `Kick` returns
`ErrServerClosed` without blocking.

---

## 6. Metrics

wspulse/server exposes an optional `MetricsCollector` interface for
instrumentation. The default is `NoopCollector{}`, which discards all
events at zero cost (the compiler inlines value-receiver no-ops).

### Configuration

```go
server.NewServer(connect,
    server.WithMetrics(myCollector),  // custom implementation
)
```

If `WithMetrics` is not called, the server uses `NoopCollector`.

### Interface

`MetricsCollector` defines typed methods for each lifecycle event.
All methods are fire-and-forget (no return value). Implementations
must be safe for concurrent use.

### Goroutine call sites

| Method                  | Called from         |
| ----------------------- | ------------------- |
| `RoomCreated`           | hub goroutine       |
| `RoomDestroyed`         | hub goroutine       |
| `ConnectionOpened`      | hub goroutine       |
| `ConnectionClosed`      | hub goroutine       |
| `ResumeAttempt`         | hub goroutine       |
| `MessageBroadcast`      | hub goroutine       |
| `MessageReceived`       | readPump goroutine  |
| `PongTimeout`           | readPump goroutine  |
| `MessageSent`           | writePump goroutine |
| `SendBufferUtilization` | writePump goroutine |
| `FrameDropped`          | hub, readPump, writePump, or transition goroutine (multiple call sites) |

### Connection duration

`ConnectionClosed` receives a `duration` parameter computed as
`time.Since(session.connectedAt)`. The `connectedAt` timestamp is set
once when the session is created in `handleRegister`. This means the
duration reflects the **logical session lifetime**, including any time
spent in the suspended state during session resumption.
