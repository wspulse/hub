# Changelog

## [Unreleased]

### Added

- `MetricsCollector` interface — typed instrumentation hooks for connection lifecycle, room state, throughput, backpressure, and heartbeat health
- `NoopCollector` — default minimal-overhead implementation that discards all events; embed it in custom implementations for forward-compatible additions
- `WithMetrics(mc MetricsCollector)` server option — plug in any metrics backend (Prometheus, OTel, or custom)
- `PanicError` exported type — wraps panics recovered from `OnMessage` handlers with the panic value and goroutine stack trace; delivered to `OnDisconnect` when resumption is disabled, or to `OnTransportDrop` when resumption is enabled (in which case `OnDisconnect` may later fire with a nil error); use `errors.As` on whichever callback error is non-nil to distinguish handler panics from transport failures
- `WithUpgraderBufferSize(readSize, writeSize int)` option — configures the WebSocket upgrader I/O buffer sizes (default 1024 bytes each)
- `DisconnectReason` type and constants (`DisconnectNormal`, `DisconnectKick`, `DisconnectGraceExpired`, `DisconnectServerClose`, `DisconnectDuplicate`) — `ConnectionClosed` now includes a reason parameter so metrics backends can distinguish disconnect causes
- Benchmarks for ring buffer, broadcast fan-out, direct Send throughput, and drop-oldest backpressure

### Changed

- Broadcast fan-out reuses a scratch slice on the hub instead of allocating a new snapshot per invocation — zero-alloc steady state since the hub event loop is single-threaded
- `ConnectFunc` GoDoc now documents that `roomID` is ignored on session resumption
- `Server.Close()` now emits `ConnectionClosed` (with `DisconnectServerClose` reason) and `RoomDestroyed` metrics for all sessions during shutdown

### Fixed

- `ServeHTTP` no longer leaks the `ConnectFunc` error message to the HTTP 401 response body — returns a generic `"unauthorized"` string instead

## [0.4.0] - 2026-03-24

### Added

- `WithOnTransportDrop(fn)` callback — fires when a connection's transport dies and the session enters the suspended state (requires `WithResumeWindow` > 0)
- `WithOnTransportRestore(fn)` callback — fires when a suspended session resumes after a client reconnects within the resume window (requires `WithResumeWindow` > 0)

### Changed

- `WithResumeWindow` no longer enforces a 3-minute upper bound — any non-negative `time.Duration` is accepted

---

## [0.3.0] - 2026-03-13

### Changed

- Package name changed from `server` to `wspulse` — import path unchanged (`github.com/wspulse/server`), but the default identifier is now `wspulse.NewServer`, `wspulse.Server`, `wspulse.Connection`, etc. Consumers using the old bare import must add an alias (`server "github.com/wspulse/server"`) or update references. (**breaking**)

---

## [0.2.1] - 2026-03-12

### Changed

- `WithResumeWindow` parameter changed from `int` (seconds) to `time.Duration` — e.g. `WithResumeWindow(30 * time.Second)` (**breaking**)

---

## [0.2.0] - 2026-03-12

### Changed

- Bump `github.com/wspulse/core` to v0.2.0
- `Frame.Event` (renamed from `Frame.Type`) and wire key `"event"` (renamed from `"type"`) — follows core v0.2.0 breaking change (**breaking**)
- Added router integration section to README

### Fixed

- `Connection.Close()` on a suspended session now immediately cancels the grace timer and fires `OnDisconnect`; previously the callback was delayed until the grace window expired
- `removeSession` now bumps `suspendEpoch` to prevent `OnDisconnect` from double-firing when `Kick` races with a simultaneously-expiring grace timer
- `handleRegister` no longer silently drops `OnDisconnect` when a reconnect races a `Connection.Close()` on a suspended session
- `disconnectSession` now cancels the grace timer before calling `Close()`, preventing a spurious `graceExpiredMessage` from being enqueued on the hub channel

---

## [0.1.0] - 2026-03-10

### Added

- `Server` with `NewServer(connect ConnectFunc, options ...ServerOption) Server`
- `Server.Send(connectionID string, frame Frame) error`
- `Server.Broadcast(roomID string, frame Frame) error`
- `Server.GetConnections(roomID string) []Connection`
- `Server.Close()` — synchronous; waits for all internal goroutines to exit
- `Connection` interface: `ID()`, `RoomID()`, `Send(Frame)`, `Close()`, `Done()`
- Session resumption: clients reconnect within `WithResumeWindow` without losing queued frames
- `WithOnConnect`, `WithOnMessage`, `WithOnDisconnect` callbacks
- `WithHeartbeat(pingPeriod, pongWait)`, `WithWriteWait`, `WithMaxMessageSize`, `WithSendBufferSize`
- `WithResumeWindow(seconds int)` — configures session resumption window (max 180 s)
- `WithCodec(codec)`, `WithCheckOrigin(fn)`, `WithLogger(l *zap.Logger)` options
- `ErrConnectionNotFound`, `ErrDuplicateConnectionID`, `ErrServerClosed` sentinel errors

### Fixed

- `Server.Close` is synchronous — returns only after all goroutines exit
- Data race in `attachWS` buffer length check

[Unreleased]: https://github.com/wspulse/server/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/wspulse/server/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/wspulse/server/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/wspulse/server/releases/tag/v0.1.0
