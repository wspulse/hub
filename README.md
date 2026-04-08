# wspulse/server

[![CI](https://github.com/wspulse/server/actions/workflows/ci.yml/badge.svg)](https://github.com/wspulse/server/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/wspulse/server.svg)](https://pkg.go.dev/github.com/wspulse/server)
[![Go](https://img.shields.io/badge/Go-1.26-blue.svg?logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A minimal, production-ready WebSocket server for Go with room-based routing, session resumption, and pluggable codecs.

**Status:** v0 — API is being stabilized. Module path: `github.com/wspulse/server`. Package name: `wspulse`.

---

## Design Goals

- Thin transport layer: no business logic, no auth, no message history
- Three public concepts: `Hub`, `Connection`, `Frame`
- Plug in any HTTP router via `http.Handler`
- Swappable codecs: JSON (default) or binary (e.g. Protobuf)
- Transparent session resumption with configurable grace window

---

## Install

```bash
go get github.com/wspulse/server
```

---

## Quick Start

```go
import "github.com/wspulse/server" // package name: wspulse

srv := wspulse.NewHub(
    // ConnectFunc: authenticate and assign room + connection IDs
    func(r *http.Request) (roomID, connectionID string, err error) {
        token := r.URL.Query().Get("token")
        userID, err := myauth.Verify(token)
        if err != nil {
            return "", "", err // → HTTP 401
        }
        return r.URL.Query().Get("room"), userID, nil
    },
    wspulse.WithOnMessage(func(connection wspulse.Connection, f wspulse.Frame) {
        // echo back to the whole room
        srv.Broadcast(connection.RoomID(), f)
    }),
    wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
        log.Printf("disconnected: %s", connection.ID())
    }),
    wspulse.WithHeartbeat(10*time.Second, 30*time.Second),
    wspulse.WithResumeWindow(30*time.Second),
)

// Standard library
http.Handle("/ws", srv)

// Gin
router.GET("/ws", func(c *gin.Context) {
    srv.ServeHTTP(c.Writer, c.Request)
})
```

### Hub-initiated push

```go
// Send to a specific connection
srv.Send(connectionID, wspulse.Frame{Event: "sys", Payload: []byte(`{"event":"welcome"}`)})

// Broadcast to a room
payload, _ := json.Marshal(myMessage)
srv.Broadcast(roomID, wspulse.Frame{Event: "msg", Payload: payload})

// Kick a connection
srv.Kick(connectionID)

// List connections in a room
connections := srv.GetConnections(roomID)
```

### Event Routing with `core/router`

`core/router` provides Gin-style middleware and per-event handler dispatch. `wspulse.Connection` satisfies `router.Connection` via structural subtyping — no adaptor needed.

```go
import (
    "github.com/wspulse/server" // package name: wspulse
    "github.com/wspulse/core/router"
)

rtr := router.New()
rtr.Use(router.Recovery()) // recover from panics in handlers

rtr.On("chat.message", func(c *router.Context) {
    srv.Broadcast(c.Connection.RoomID(), c.Frame)
})
rtr.On("chat.join", func(c *router.Context) {
    welcome := wspulse.Frame{Event: "chat.welcome", Payload: []byte(`"hello"`)}
    _ = c.Connection.Send(welcome)
})

var srv wspulse.Hub
srv = wspulse.NewHub(
    connectFunc,
    wspulse.WithOnMessage(func(conn wspulse.Connection, f wspulse.Frame) {
        rtr.Dispatch(conn, f)
    }),
)
```

See [wspulse/core](https://github.com/wspulse/core) for the full `router` API.

---

## Public API Surface

| Symbol        | Description                                                             |
| ------------- | ----------------------------------------------------------------------- |
| `Hub`         | Manages sessions, heartbeats, and room routing                          |
| `Connection`  | A logical WebSocket session (`ID`, `RoomID`, `Send`, `Close`, `Done`)   |
| `Frame`       | Transport unit (`Event`, `Payload []byte`) — re-exported from core       |
| `ConnectFunc` | `func(*http.Request) (roomID, connectionID string, err error)`          |
| `Codec`       | Interface: `Encode(Frame)`, `Decode([]byte)`, `FrameType()` — from core |
| `JSONCodec`   | Default codec — text frames, JSON payload — re-exported from core       |

### Hub options

| Option                      | Default                              |
| --------------------------- | ------------------------------------ |
| `WithOnConnect(fn)`         | —                                    |
| `WithOnMessage(fn)`         | —                                    |
| `WithOnDisconnect(fn)`      | —                                    |
| `WithOnTransportDrop(fn)`   | —                                    |
| `WithOnTransportRestore(fn)`| —                                    |
| `WithResumeWindow(d)`        | 0 (disabled)                         |
| `WithHeartbeat(ping, pong)` | 10 s / 30 s                          |
| `WithWriteWait(d)`          | 10 s                                 |
| `WithMaxMessageSize(n)`     | 512 B                                |
| `WithSendBufferSize(n)`     | 256 frames                           |
| `WithCodec(c)`              | JSONCodec                            |
| `WithCheckOrigin(fn)`       | allow all                            |
| `WithLogger(l)`             | zap.NewNop() — accepts `*zap.Logger` |
| `WithMetrics(mc)`           | NoopCollector — accepts `MetricsCollector` |

---

## Features

- **Room-based routing** — connections are partitioned into rooms; broadcast targets a single room.
- **Pluggable auth** — `ConnectFunc` runs during HTTP Upgrade, before any WebSocket frames are exchanged.
- **Session resumption** — opt-in via `WithResumeWindow(d)`. When a transport drops, the session is suspended for `d` before firing `OnDisconnect`. If the client reconnects with the same `connectionID` within that window, the new WebSocket is swapped in transparently — no `OnConnect` / `OnDisconnect` callbacks fire, and buffered frames are replayed in order. Disabled by default.
- **Automatic heartbeat** — server-side Ping / Pong with configurable intervals (`WithHeartbeat`).
- **Backpressure** — bounded per-connection send buffer; oldest frame is dropped on overflow during broadcast.
- **Swappable codec** — JSON by default; implement the `Codec` interface to plug in any encoding (binary, Protobuf, MessagePack, etc.).
- **Kick** — `Hub.Kick(connectionID)` always destroys the session immediately, bypassing the resume window.
- **Graceful shutdown** — `Hub.Close()` sends close frames to all connected clients, drains in-flight registrations, and fires `OnDisconnect` for every session.
- **Metrics** — optional `MetricsCollector` interface for observability; default is `NoopCollector` (minimal overhead). See [Metrics](#metrics) below.

---

## Metrics

wspulse/server exposes a `MetricsCollector` interface with typed hooks for connection lifecycle, room state, throughput, backpressure, and heartbeat health. The default is `NoopCollector{}` (minimal overhead). Embed `NoopCollector` in custom implementations for forward-compatible additions.

```go
// Use a contrib adapter (e.g. wspulse/metrics-prometheus or wspulse/metrics-otel),
// or implement MetricsCollector yourself.
var collector wspulse.MetricsCollector = myCollector

srv := wspulse.NewHub(connect,
    wspulse.WithMetrics(collector),
)
```

The interface covers:

| Hook | Description |
| --- | --- |
| `ConnectionOpened` / `ConnectionClosed` | Session lifecycle with duration and disconnect reason |
| `RoomCreated` / `RoomDestroyed` | Room allocation and deallocation |
| `MessageReceived` / `MessageSent` / `MessageBroadcast` | Throughput with byte sizes and fan-out |
| `FrameDropped` / `SendBufferUtilization` | Backpressure visibility |
| `ResumeAttempt` | Session resumption tracking |
| `PongTimeout` | Heartbeat health |

Implement `MetricsCollector` to plug in any backend (Prometheus, OTel, or custom). See [doc/internals.md](doc/internals.md) for goroutine call sites and thread safety requirements.

---

## Frame Routing with `core/router`

The router from [wspulse/core](https://github.com/wspulse/core) integrates directly with `WithOnMessage`. It dispatches each incoming frame to its handler based on the **`"event"` field** in the JSON message:

```json
{
  "event": "chat.message",
  "payload": { "text": "hello" }
}
```

The value of `"event"` is `frame.Event` on the Go side, and is the key used to select the handler.

```go
import (
    "github.com/wspulse/server" // package name: wspulse
    "github.com/wspulse/core/router"
)

var srv wspulse.Hub

r := router.New()
r.Use(router.Recovery())
r.Use(func(c *router.Context) {
    // middleware runs before every handler
    c.Set("roomID", c.Connection.RoomID())
    c.Next()
})

// matches frames where "event" == "chat.message"
r.On("chat.message", func(c *router.Context) {
    srv.Broadcast(c.Connection.RoomID(), wspulse.Frame{
        Event:   "chat.message",
        Payload: c.Frame.Payload,
    })
})

// matches frames where "event" == "ping"
r.On("ping", func(c *router.Context) {
    _ = c.Connection.Send(wspulse.Frame{Event: "pong"})
})

srv = wspulse.NewHub(
    connectFn,
    wspulse.WithOnMessage(func(conn wspulse.Connection, f wspulse.Frame) {
        r.Dispatch(conn, f)
    }),
)
```

## Development

```bash
make fmt        # auto-format source files (gofmt + goimports)
make check      # validate format, lint, test with race detector (pre-commit gate)
make test       # go test -race -count=3 ./...
make test-cover # go test with coverage report → coverage.html
make bench      # run benchmarks with memory allocation stats
make tidy       # go mod tidy (GOWORK=off)
make clean      # remove build artifacts and test cache
```

---

## Related Modules            
 
- [wspulse/core](https://github.com/wspulse/core) - Shared types and codecs 
- [wspulse/client-go](https://github.com/wspulse/client-go) - Go client library with automatic reconnection and session resumption
