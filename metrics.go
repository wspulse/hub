package wspulse

import "time"

// DisconnectReason describes why a connection was closed.
// Used as a label-friendly parameter in ConnectionClosed to let metrics
// backends (Prometheus, OTel, etc.) distinguish disconnect causes.
type DisconnectReason string

const (
	// DisconnectNormal indicates the connection was closed normally:
	// the transport died (no resume configured), or the application
	// called Connection.Close() on a suspended session.
	DisconnectNormal DisconnectReason = "normal"

	// DisconnectKick indicates the connection was terminated by
	// an explicit Server.Kick() call.
	DisconnectKick DisconnectReason = "kick"

	// DisconnectGraceExpired indicates the resume window elapsed
	// without the client reconnecting.
	DisconnectGraceExpired DisconnectReason = "grace_expired"

	// DisconnectServerClose indicates the connection was terminated
	// because Server.Close() shut down the server.
	DisconnectServerClose DisconnectReason = "server_close"

	// DisconnectDuplicate indicates the connection was replaced by
	// a new connection with the same connectionID.
	DisconnectDuplicate DisconnectReason = "duplicate"
)

// MetricsCollector defines instrumentation hooks for wspulse server.
// Each method corresponds to a single lifecycle or throughput event.
//
// Implementations must be safe for concurrent use. Methods are called from
// the hub goroutine, readPump goroutines, and writePump goroutines.
//
// All methods are fire-and-forget: they do not return values. If the
// underlying metrics backend encounters an error, the implementation
// should handle it internally (e.g. log and skip).
//
// Hooks are invoked synchronously on hot paths; implementations must
// return quickly and must not panic. Implementations must not call back
// into the same Server synchronously (e.g. Kick, Send, Broadcast) as
// this can deadlock the hub event loop.
//
// For forward-compatible custom implementations, embed NoopCollector:
//
//	type MyCollector struct {
//	    wspulse.NoopCollector // provides no-op defaults for future methods
//	}
//	func (c *MyCollector) ConnectionOpened(roomID, connectionID string) {
//	    // custom implementation
//	}
//
// This ensures new methods added to MetricsCollector in future
// versions are automatically satisfied by the embedded no-op defaults.
type MetricsCollector interface {
	// ConnectionOpened is called when a new session is created and registered.
	ConnectionOpened(roomID, connectionID string)

	// ConnectionClosed is called when a session is terminated. duration is the
	// total logical session lifetime from creation to destruction, including
	// any time spent in the suspended state. reason indicates why the session
	// was closed (see DisconnectReason constants).
	ConnectionClosed(roomID, connectionID string, duration time.Duration, reason DisconnectReason)

	// ResumeAttempt is called when a client attempts to resume a suspended session.
	// success is true when the session was successfully resumed (transport swap),
	// and false when the session no longer exists (grace window expired).
	//
	// A failed resume (success=false) is emitted in two scenarios:
	//   - ServeHTTP pre-check: client connects with ?resume=true but
	//     hub.get(connectionID) returns nil. HTTP 410 is returned.
	//   - Hub race case: pre-check passed but the session expired between
	//     the check and hub processing. WS close 4100 is sent.
	//
	// In both failure cases, no session is created and ConnectionOpened is not emitted.
	ResumeAttempt(roomID, connectionID string, success bool)

	// RoomCreated is called when the first connection joins a room,
	// causing the room to be allocated.
	RoomCreated(roomID string)

	// RoomDestroyed is called when the last connection leaves a room,
	// causing the room to be deallocated.
	RoomDestroyed(roomID string)

	// MessageReceived is called in the readPump when an inbound WebSocket
	// message is read, before decoding. sizeBytes is the raw wire size.
	MessageReceived(roomID string, sizeBytes int)

	// MessageBroadcast is called after a broadcast frame is fanned out to
	// all sessions in a room. sizeBytes is the pre-encoded frame size;
	// fanOut is the number of recipient sessions (including suspended ones).
	MessageBroadcast(roomID string, sizeBytes int, fanOut int)

	// MessageSent is called in the writePump after a frame is successfully
	// written to the WebSocket transport. sizeBytes is the encoded frame size.
	MessageSent(roomID, connectionID string, sizeBytes int)

	// FrameDropped is called whenever a frame is discarded due to send buffer
	// backpressure. This covers drops from Send (buffer full), Broadcast
	// (drop-oldest), and resume drain (buffer full during replay).
	// In the drop-oldest path, two FrameDropped events may fire: one for the
	// oldest frame evicted and one for the new frame if it still cannot be
	// enqueued — both represent real frame loss.
	FrameDropped(roomID, connectionID string)

	// SendBufferUtilization is called in the writePump after every successful
	// write. used and capacity report the current send channel occupancy.
	//
	// This method is called once per message write. For high-throughput
	// connections (e.g. 10k msg/s), expect the same call rate per connection.
	// Implementations should apply sampling, batching, or throttling as needed.
	SendBufferUtilization(roomID, connectionID string, used, capacity int)

	// PongTimeout is called in the readPump when a read deadline expires,
	// indicating the remote peer failed to respond to a Ping in time.
	PongTimeout(roomID, connectionID string)
}

// NoopCollector is the default MetricsCollector that discards all events.
// All methods are value-receiver no-ops with minimal overhead.
//
// Embed NoopCollector in custom implementations for forward-compatible
// additions to the MetricsCollector interface:
//
//	type MyCollector struct {
//	    wspulse.NoopCollector
//	}
type NoopCollector struct{}

// compile-time check: NoopCollector must satisfy MetricsCollector.
var _ MetricsCollector = NoopCollector{}

// ConnectionOpened is a no-op. See MetricsCollector.ConnectionOpened.
func (NoopCollector) ConnectionOpened(_, _ string) {}

// ConnectionClosed is a no-op. See MetricsCollector.ConnectionClosed.
func (NoopCollector) ConnectionClosed(_, _ string, _ time.Duration, _ DisconnectReason) {}

// ResumeAttempt is a no-op. See MetricsCollector.ResumeAttempt.
func (NoopCollector) ResumeAttempt(_, _ string, _ bool) {}

// RoomCreated is a no-op. See MetricsCollector.RoomCreated.
func (NoopCollector) RoomCreated(_ string) {}

// RoomDestroyed is a no-op. See MetricsCollector.RoomDestroyed.
func (NoopCollector) RoomDestroyed(_ string) {}

// MessageReceived is a no-op. See MetricsCollector.MessageReceived.
func (NoopCollector) MessageReceived(_ string, _ int) {}

// MessageBroadcast is a no-op. See MetricsCollector.MessageBroadcast.
func (NoopCollector) MessageBroadcast(_ string, _ int, _ int) {}

// MessageSent is a no-op. See MetricsCollector.MessageSent.
func (NoopCollector) MessageSent(_, _ string, _ int) {}

// FrameDropped is a no-op. See MetricsCollector.FrameDropped.
func (NoopCollector) FrameDropped(_, _ string) {}

// SendBufferUtilization is a no-op. See MetricsCollector.SendBufferUtilization.
func (NoopCollector) SendBufferUtilization(_, _ string, _, _ int) {}

// PongTimeout is a no-op. See MetricsCollector.PongTimeout.
func (NoopCollector) PongTimeout(_, _ string) {}
