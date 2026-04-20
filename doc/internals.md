# wspulse/hub — Transport Layer Internals

This document describes the internal implementation mechanisms of wspulse/hub
at the **transport and infrastructure layer**. Application-level semantics
(business logic, room state, message history) are out of scope here; those
belong to the consumers of wspulse/hub.

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

Every accepted WebSocket connection spawns exactly **three pump goroutines**
plus one dedicated **bridge goroutine**:

```
attachWS(transport)
  ├─ go bridge          — propagates close(session.done) → pumpCancel()
  ├─ go readPump(ctx)   — reads inbound messages, calls OnMessage, signals heart on exit
  ├─ go writePump(ctx)  — drains the session send queue, sole writer of application data on transport
  └─ go pingPump(ctx)   — drives Ping heartbeat, fires HeartbeatFailed on failure
```

All pump goroutines share a single `pumpCtx` (derived from
`context.Background()`) and are cancelled together via `pumpCancel()`.
None of these goroutines are accessible to the application layer; the
`Connection` interface exposes only `Send`, `Close`, and `Done`.

### Bridge goroutine

The bridge goroutine links the session lifetime (`session.done`) to the pump
lifetime (`pumpCtx`). When `session.Close()` closes `done`, the bridge calls
`pumpCancel()`, causing all three pumps to exit. If `pumpCtx` is cancelled
first (e.g. by `detachWS` during resume), the bridge exits without effect.

### Goroutine exit and cleanup

- `readPump` owns the transport-died signal: when `transport.Read(ctx)` returns
  an error (normal close, network drop, or context cancellation) `readPump`
  sends a `transportDied` message to the heart and exits.
- `writePump` owns the TCP connection close call: it calls `transport.CloseNow()`
  on exit (via defer) to ensure the underlying socket is always released.
  On graceful shutdown (`ctx.Done()`), it first sends a close frame via
  `transport.Close(StatusNormalClosure, "")`.
- `pingPump` sends periodic Pings with a `writeTimeout` timeout. On failure it
  fires the `HeartbeatFailed` metric and calls `transport.CloseNow()` to force the
  transport closed, causing `readPump` to detect the error and signal the heart.
  An initial ping is sent immediately on startup to detect dead-on-arrival
  connections without waiting a full `pingInterval`.
- `session.done` is a `chan struct{}` closed exactly once (via `closeOnce`) by
  `session.Close()`. The bridge goroutine translates this into `pumpCancel()`
  so all pumps exit via context cancellation.
- `session.send` is a `*sendQueue` (mutex-guarded `ring.Buffer` + `sync.Cond`)
  shared across reconnects for the lifetime of the session. It is closed by
  `session.Close()` after `done` is closed, which unblocks any goroutine
  blocked in `sendQueue.Pop`. This eliminates the TOCTOU race present in the
  previous three-select drop-oldest pattern.
- `writePump` calls `s.send.Pop(pumpCtx)`, which blocks until a message is
  available, the context is cancelled, or the queue is closed.

---

## 2. Heartbeat Mechanism

Heartbeats use **RFC 6455 protocol-layer Ping / Pong control frames**,
completely separate from application JSON messages. The heartbeat is driven
by a dedicated `pingPump` goroutine using `coder/websocket`'s synchronous
`Ping(ctx)` API, which sends a Ping frame and blocks until the Pong reply
arrives or the context expires.

### Parameters

| Parameter        | Default | Valid range          | Description                                                           |
| ---------------- | ------- | -------------------- | --------------------------------------------------------------------- |
| `pingInterval`   | 20 s    | (writeTimeout, 1 m]  | `pingPump` ticker interval; one synchronous Ping sent per tick        |
| `writeTimeout`   | 10 s    | (0, 30 s]            | Per-write deadline and Ping timeout (context timeout for `Ping(ctx)`) |
| `maxMessageSize` | 512 B   | [1, 64 MiB]          | `readPump SetReadLimit`; exceeded size triggers immediate disconnect   |
| Send buffer      | 256     | [1, 4096]            | `session.send` queue capacity (configurable via `WithSendBufferSize`) |

**Constraint:** `pingInterval` must be strictly greater than `writeTimeout`.
`NewHub` panics at construction time if this constraint is violated.
This ensures the Pong for ping N always resolves before the ticker fires for ping N+1
on a healthy connection, preventing tick accumulation in the `pingPump` select loop.

Configuring non-default values:

```go
wspulse.NewHub(connect,
    wspulse.WithPingInterval(30*time.Second),
    wspulse.WithWriteTimeout(15*time.Second),
    wspulse.WithMaxMessageSize(4096),
)
```

### Operational details

1. **Ping dispatch** — `pingPump` calls `transport.Ping(ctx)` with a
   `context.WithTimeout(pumpCtx, writeTimeout)`. The call blocks until the Pong
   arrives or the timeout fires. An initial ping is sent immediately on
   startup (before the first ticker tick) to detect dead-on-arrival
   connections.

2. **Timeout disconnect** — If `Ping(ctx)` returns an error (timeout or
   network failure), `pingPump` fires the `HeartbeatFailed` metric and calls
   `transport.CloseNow()` to force-close the underlying connection. This
   causes `readPump`'s `Read(ctx)` to return an error, which triggers the
   standard teardown path via `transportDiedMessage`.

3. **Client-side Pong** — Standard WebSocket implementations (browsers,
   `coder/websocket`) reply to Ping automatically; the application layer does
   not need to handle this. Native clients may also send their own Ping for
   client-side dead-connection detection; `coder/websocket` auto-replies to
   these.

---

## 3. Backpressure and Send Buffer

Each session maintains a `session.send *sendQueue` — a mutex-guarded
`ring.Buffer[[]byte]` with a `sync.Cond` for blocking `Pop`. Its capacity
is configurable (default **256**). When `Hub.Broadcast` or `Hub.Send` is called:

1. The encoded message bytes are passed to `sendQueue.Enqueue` or
   `sendQueue.ForceEnqueue` (both hold the mutex for the entire operation).
2. If the buffer is **full**, `ErrSendBufferFull` is returned to the caller
   (for direct `Send`, via `Enqueue`) or **drop-oldest** backpressure is applied
   atomically (for `Broadcast`, via `ForceEnqueue`): the oldest message is evicted
   and the new message is inserted in a single critical section — no TOCTOU race.
3. When `resumeWindow > 0` and the session is suspended (no active WebSocket),
   messages are buffered to an in-memory `ring.Buffer` instead of the send queue.
   These messages are replayed when the client reconnects.

This ensures a slow or lagging connection cannot block the heart event loop or
stall broadcasts to other healthy connections.

If the application needs reliable delivery for a specific connection, it should
call `connection.Send` directly and handle `ErrSendBufferFull` at the call
site — for example, by scheduling a retry or incrementing a dropped-messages
metric.

---

## 4. Connection Teardown

Normal and abnormal teardown follow the same cleanup path:

```
cause (close frame / network drop / ping timeout)
  → pingPump: Ping fails → CloseNow() → readPump's Read unblocks
  → readPump: Read(ctx) returns error, sends transportDiedMessage to heart
  → heart: if resumeWindow > 0 → suspend session (start grace timer)
           if resumeWindow == 0 → remove session, call OnDisconnect
  → session.Close() (via closeOnce): closes done channel
  → bridge goroutine: <-done → pumpCancel()
  → writePump: ctx.Done() fires, sends Close(StatusNormalClosure) only
    on session shutdown (s.done closed); skipped on reconnect swap
    and if the priority-exit path runs first. Defers CloseNow()
```

Explicit teardown via `Hub.Kick(connectionID)` takes a different path — the
kick request is routed through the heart to ensure serialized cleanup (see
§5 "Kick Bypass" for details).

The `closeOnce sync.Once` guard ensures `close(done)` executes exactly once
regardless of which side (readPump, writePump, grace timer expiry, or an
explicit `Hub.Kick` call) initiates the teardown.

---

## 5. Session Resumption

When `WithResumeWindow(d)` is configured with `d > 0`, wspulse/hub
introduces a **session layer** that decouples the application-visible
`Connection` from the underlying WebSocket transport. This allows transparent
reconnection without leaking connect/disconnect events to the application
layer.

> **Type:** `WithResumeWindow` accepts a `time.Duration`.
> `WithResumeWindow(30 * time.Second)` means a 30-second grace window.
> Valid range: 0 (disabled) … no upper limit.

### Architecture

`Connection` (public interface) is implemented by `session` (private struct).
The session holds a `core.Transport` (`transport`) representing the current
physical connection. When the WebSocket dies, the session enters a
**suspended** state and starts a grace timer. If the client reconnects with
the same `connectionID` before the timer expires, the new WebSocket is
swapped in silently.

### Session State Machine

```
[*] → Connected : handleRegister creates session + transport

Connected → Suspended : transport dies, resumeWindow > 0 (start timer, buffer messages, onTransportDrop fires)
Connected → Closed    : transport dies, resumeWindow == 0 (onDisconnect fires)
Connected → Closed    : Kick() or Close() (onDisconnect fires)
Connected → Closed    : duplicate connectionID arrives (old session kicked, onDisconnect fires with ErrDuplicateConnectionID; new session created)

Suspended → Connected : same connectionID reconnect (cancel timer, replay buffer, onTransportRestore fires)
Suspended → Closed    : timer expires (onDisconnect fires, session destroyed)
Suspended → Closed    : Connection.Close() called (cancel timer, onDisconnect fires immediately)
Suspended → Closed    : Kick() or hub.Close() (cancel timer, onDisconnect fires immediately)

Closed → [*]
```

### WebSocket Swap Sequence

```
WS1 connection drops
  → WS1 sends transportDiedMessage(session, err) to Heart
  → Heart detaches WS1, starts resumeWindow timer
  → go onTransportDrop(session, err)
  → Session state = suspended, messages buffered to ringBuffer

WS2 client reconnects with same connectionID
  → WS2 sends register(connectionID, transport) to Heart
  → Heart cancels timer
  → Heart attaches WS2, drains ringBuffer to send queue
  → Heart starts readPump(WS2) + writePump(WS2) + pingPump(WS2)
  → onTransportRestore fires; no onConnect / onDisconnect fired
```

Note: on resume, the `roomID` returned by the new `ConnectFunc` call is
ignored. The session retains its original room assignment from the initial
connection.

### Resume Buffer

During the suspended state, messages sent via `session.Send()` or
`Hub.Broadcast()` are stored in an in-memory `ringBuffer` with a capacity
equal to `sendBufferSize` (default 256 messages). When the buffer is full, the
oldest message is dropped (same backpressure strategy as the send queue during
normal operation).

On reconnect, buffered messages are drained from the ring buffer into the send
queue before the new `writePump` starts, ensuring ordering is preserved.

### Effective Reconnect Window

The total time a client has to reconnect is:

```
effective window = pingInterval + writeTimeout + resumeWindow
```

- `pingInterval` (default 20 s): worst-case wait until the next ping fires.
- `writeTimeout` (default 10 s): Ping timeout before the server detects the dead
  transport.
- `resumeWindow` (configured via `WithResumeWindow`): additional grace period
  after detection.

The client's exponential backoff reconnect (1 s, 2 s, 4 s, 8 s, ...) can
attempt multiple retries within this window.

### Kick Bypass

`Hub.Kick(connectionID)` always destroys the session immediately, bypassing
the resume window. Kick is an explicit application-layer action that signals
intentional removal, not a transient network failure.

#### Heart-routed Kick

Kick is routed through the heart's event loop via a `kickRequest` channel.
This ensures that map removal, `session.Close()`, and the `OnDisconnect`
callback are all serialized with other state mutations (register,
transportDied, graceExpired). Without this, calling `Close()` directly on a
**suspended** session would close `session.done` but leave the session in heart
maps until the grace timer fires.

To prevent `Kick` from double-firing `onDisconnect` when a grace timer happens
to fire simultaneously, `removeSession` (called by `handleKick`) bumps
`suspendEpoch`. The stale grace message is then discarded by
`handleGraceExpired`'s epoch check.

```
Kick(connectionID) → kickRequest{connectionID, result} → heart.kick channel
heart.run() selects kickRequest
  → handleKick: removeSession + session.Close() + go onDisconnect()
  → writes nil to result channel
Kick() returns nil
```

If the heart has already shut down (`<-heart.done`), `Kick` returns
`ErrHubClosed` without blocking.

---

## 6. Metrics

wspulse/hub exposes an optional `MetricsCollector` interface for
instrumentation. The default is `NoopCollector{}`, a no-op implementation
that discards all events with minimal overhead.

### Configuration

```go
wspulse.NewHub(connect,
    wspulse.WithMetrics(myCollector),  // custom implementation
)
```

If `WithMetrics` is not called, the Hub uses `NoopCollector`.

### Interface

`MetricsCollector` defines typed methods for each lifecycle event.
All methods are fire-and-forget (no return value). Implementations
must be safe for concurrent use.

### Goroutine call sites

| Method                  | Called from         |
| ----------------------- | ------------------- |
| `RoomCreated`           | heart goroutine       |
| `RoomDestroyed`         | heart goroutine       |
| `ConnectionOpened`      | heart goroutine       |
| `ConnectionClosed`      | heart goroutine       |
| `ResumeAttempt`         | heart goroutine       |
| `MessageBroadcast`      | heart goroutine       |
| `MessageReceived`       | readPump goroutine  |
| `HeartbeatFailed`      | pingPump goroutine  |
| `MessageSent`           | writePump goroutine |
| `SendBufferUtilization` | writePump goroutine |
| `MessageDropped`        | heart goroutine (broadcast), caller goroutine (Send), or transition goroutine (resume drain) |

### Connection duration

`ConnectionClosed` receives a `duration` parameter computed as
`time.Since(session.connectedAt)`. The `connectedAt` timestamp is set
once when the session is created in `handleRegister`. This means the
duration reflects the **logical session lifetime**, including any time
spent in the suspended state during session resumption.

`ConnectionClosed` also receives a `reason` parameter of type
`DisconnectReason` that indicates why the session was terminated.
See the `DisconnectReason` constants for possible values.
