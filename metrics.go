package wspulse

import "time"

// MetricsCollector defines instrumentation hooks for wspulse server.
// Each method corresponds to a single lifecycle or throughput event.
//
// Implementations must be safe for concurrent use. Methods are called from
// the hub goroutine, readPump goroutines, and writePump goroutines.
//
// All methods are fire-and-forget: they do not return values. If the
// underlying metrics backend encounters an error, the implementation
// should handle it internally (e.g. log and skip).
type MetricsCollector interface {
	// ConnectionOpened is called when a new session is created and registered.
	ConnectionOpened(roomID, connectionID string)

	// ConnectionClosed is called when a session is terminated. duration is the
	// total logical session lifetime from creation to destruction, including
	// any time spent in the suspended state.
	ConnectionClosed(roomID, connectionID string, duration time.Duration)

	// ResumeAttempt is called when a suspended session is successfully resumed
	// by a reconnecting client. Currently only emitted on success (success=true)
	// because a failed resume (reconnect after grace expiry) results in a new
	// session rather than an identifiable failed-resume event.
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
	FrameDropped(roomID, connectionID string)

	// SendBufferUtilization is called in the writePump after every successful
	// write. used and capacity report the current send channel occupancy.
	// Adapter implementations may apply sampling or throttling as needed.
	SendBufferUtilization(roomID, connectionID string, used, capacity int)

	// PongTimeout is called in the readPump when a read deadline expires,
	// indicating the remote peer failed to respond to a Ping in time.
	PongTimeout(roomID, connectionID string)
}

// NoopCollector is the default MetricsCollector that discards all events.
// All methods are no-ops on a value receiver, allowing the compiler to
// inline them at zero cost.
type NoopCollector struct{}

// compile-time check: NoopCollector must satisfy MetricsCollector.
var _ MetricsCollector = NoopCollector{}

// ConnectionOpened is a no-op. See MetricsCollector.ConnectionOpened.
func (NoopCollector) ConnectionOpened(_, _ string) {}

// ConnectionClosed is a no-op. See MetricsCollector.ConnectionClosed.
func (NoopCollector) ConnectionClosed(_, _ string, _ time.Duration) {}

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

// WithMetrics configures the MetricsCollector used by the Server.
// Defaults to NoopCollector{} if not set.
// Panics if collector is nil.
func WithMetrics(collector MetricsCollector) ServerOption {
	if collector == nil {
		panic("wspulse: WithMetrics: collector must not be nil")
	}
	return func(c *serverConfig) { c.metrics = collector }
}
