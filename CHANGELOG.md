# Changelog

## [Unreleased]

### Changed

- Bump `github.com/wspulse/core` to v0.2.0
- `Frame.Event` (renamed from `Frame.Type`) and wire key `"event"` (renamed from `"type"`) — follows core v0.2.0 breaking change (**breaking**)
- Added router integration section to README

### Fixed

- `Connection.Close()` on a suspended session now correctly fires `OnDisconnect` when the grace timer expires; previously, the callback was silently skipped for sessions closed while suspended
- `removeSession` now bumps `suspendEpoch` to prevent `OnDisconnect` from double-firing when `Kick` races with a simultaneously-expiring grace timer

---

## [0.1.0] - 2026-03-10

### Added

- `Server` with `NewServer(connect ConnectFunc, opts ...ServerOption) *Server`
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

[Unreleased]: https://github.com/wspulse/server/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/wspulse/server/releases/tag/v0.1.0
