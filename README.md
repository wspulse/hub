# wspulse/server

A minimal, production-ready WebSocket server for Go with room-based routing, session resumption, and pluggable codecs.

**Status:** v0 — API is being stabilized. Module path: `github.com/wspulse/server`.

---

## Design Goals

- Thin transport layer: no business logic, no auth, no message history
- Three public concepts: `Server`, `Connection`, `Frame`
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
import "github.com/wspulse/server"

srv := server.NewServer(
    // ConnectFunc: authenticate and assign room + connection IDs
    func(r *http.Request) (roomID, connectionID string, err error) {
        token := r.URL.Query().Get("token")
        userID, err := myauth.Verify(token)
        if err != nil {
            return "", "", err // → HTTP 401
        }
        return r.URL.Query().Get("room"), userID, nil
    },
    server.WithOnMessage(func(connection server.Connection, f server.Frame) {
        // echo back to the whole room
        srv.Broadcast(connection.RoomID(), f)
    }),
    server.WithOnDisconnect(func(connection server.Connection, err error) {
        log.Printf("disconnected: %s", connection.ID())
    }),
    server.WithHeartbeat(10*time.Second, 30*time.Second),
    server.WithResumeWindow(30),
)

// Standard library
http.Handle("/ws", srv)

// Gin
router.GET("/ws", func(c *gin.Context) {
    srv.ServeHTTP(c.Writer, c.Request)
})
```

### Server-initiated push

```go
// Send to a specific connection
srv.Send(connectionID, server.Frame{Event: "sys", Payload: []byte(`{"event":"welcome"}`)})

// Broadcast to a room
payload, _ := json.Marshal(myMessage)
srv.Broadcast(roomID, server.Frame{Event: "msg", Payload: payload})

// Kick a connection
srv.Kick(connectionID)

// List connections in a room
connections := srv.GetConnections(roomID)
```

---

## Public API Surface

| Symbol        | Description                                                             |
| ------------- | ----------------------------------------------------------------------- |
| `Server`      | Manages sessions, heartbeats, and room routing                          |
| `Connection`  | A logical WebSocket session (`ID`, `RoomID`, `Send`, `Close`, `Done`)   |
| `Frame`       | Transport unit (`ID`, `Event`, `Payload []byte`) — re-exported from core |
| `ConnectFunc` | `func(*http.Request) (roomID, connectionID string, err error)`          |
| `Codec`       | Interface: `Encode(Frame)`, `Decode([]byte)`, `FrameType()` — from core |
| `JSONCodec`   | Default codec — text frames, JSON payload — re-exported from core       |

### Server options

| Option                      | Default                              |
| --------------------------- | ------------------------------------ |
| `WithOnConnect(fn)`         | —                                    |
| `WithOnMessage(fn)`         | —                                    |
| `WithOnDisconnect(fn)`      | —                                    |
| `WithResumeWindow(seconds)`  | 0 (disabled)                         |
| `WithHeartbeat(ping, pong)` | 10 s / 30 s                          |
| `WithWriteWait(d)`          | 10 s                                 |
| `WithMaxMessageSize(n)`     | 512 B                                |
| `WithSendBufferSize(n)`     | 256 frames                           |
| `WithCodec(c)`              | JSONCodec                            |
| `WithCheckOrigin(fn)`       | allow all                            |
| `WithLogger(l)`             | zap.NewNop() — accepts `*zap.Logger` |

---

## Features

- **Room-based routing** — connections are partitioned into rooms; broadcast targets a single room.
- **Pluggable auth** — `ConnectFunc` runs during HTTP Upgrade, before any WebSocket frames are exchanged.
- **Session resumption** — opt-in via `WithResumeWindow(seconds)`. When a transport drops, the session is suspended for `seconds` before firing `OnDisconnect`. If the client reconnects with the same `connectionID` within that window, the new WebSocket is swapped in transparently — no `OnConnect` / `OnDisconnect` callbacks fire, and buffered frames are replayed in order. Disabled by default.
- **Automatic heartbeat** — server-side Ping / Pong with configurable intervals (`WithHeartbeat`).
- **Backpressure** — bounded per-connection send buffer; oldest frame is dropped on overflow during broadcast.
- **Swappable codec** — JSON by default; implement the `Codec` interface to plug in any encoding (binary, Protobuf, MessagePack, etc.).
- **Kick** — `Server.Kick(connectionID)` always destroys the session immediately, bypassing the resume window.
- **Graceful shutdown** — `Server.Close()` sends close frames to all connected clients, drains in-flight registrations, and fires `OnDisconnect` for every session.

---

## Frame Routing with `core/router`

The router from [wspulse/core](https://github.com/wspulse/core) integrates directly with `WithOnMessage`. It dispatches each incoming frame to its handler based on the **`"event"` field** in the JSON message:

```json
{
  "id": "msg-001",
  "event": "chat.message",
  "payload": { "text": "hello" }
}
```

The value of `"event"` is `frame.Event` on the Go side, and is the key used to select the handler.

```go
import (
    "github.com/wspulse/server"
    "github.com/wspulse/core/router"
)

var srv server.Server

r := router.New()
r.Use(router.Recovery())
r.Use(func(c *router.Context) {
    // middleware runs before every handler
    c.Set("roomID", c.Connection.RoomID())
    c.Next()
})

// matches frames where "event" == "chat.message"
r.On("chat.message", func(c *router.Context) {
    srv.Broadcast(c.Connection.RoomID(), server.Frame{
        Event:   "chat.message",
        Payload: c.Frame.Payload,
    })
})

// matches frames where "event" == "ping"
r.On("ping", func(c *router.Context) {
    _ = c.Connection.Send(server.Frame{Event: "pong"})
})

srv = server.NewServer(
    connectFn,
    server.WithOnMessage(func(conn server.Connection, f server.Frame) {
        r.Dispatch(conn, f)
    }),
)
```

---

## Wire Protocol

See [doc/protocol.md](doc/protocol.md) for the JSON frame format.

## Internals

See [doc/internals.md](doc/internals.md) for the goroutine model, heartbeat mechanism, backpressure, and session resumption implementation details.

---

## Related Modules

| Module                                                    | Description             |
| --------------------------------------------------------- | ----------------------- |
| [wspulse/core](https://github.com/wspulse/core)           | Shared types and codecs |
| [wspulse/client-go](https://github.com/wspulse/client-go) | Go WebSocket client     |
