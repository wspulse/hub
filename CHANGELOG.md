# Changelog

## [Unreleased]

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
