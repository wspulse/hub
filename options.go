package wspulse

import (
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// Configuration upper bounds — option functions panic if these ceilings are exceeded.
const (
	maxPingInterval  = 1 * time.Minute  // WithPingInterval upper bound
	maxWriteTimeout  = 30 * time.Second // WithWriteTimeout upper bound
	maxMsgSizeBytes  = 64 << 20         // WithMaxMessageSize upper bound — 64 MiB
	maxSendBufFrames = 4096             // WithSendBufferSize upper bound
)

// ConnectFunc authenticates an incoming HTTP upgrade request and provides the
// roomID and connectionID for the new connection.
// Returning a non-nil error rejects the upgrade with HTTP 401.
// If connectionID is empty the Hub assigns a random UUID so that every connection
// has a unique, non-empty ID. Use a non-empty connectionID when the application needs
// deterministic IDs (e.g. for Hub.Send and Hub.Kick).
//
// On session resumption (reconnect of a suspended session within the resume
// window), the roomID returned by ConnectFunc is ignored — the session retains
// its original room assignment from the initial connection. For new sessions
// (including when a duplicate connectionID replaces an existing connected
// session), the roomID from ConnectFunc determines the room.
type ConnectFunc func(r *http.Request) (roomID, connectionID string, err error)

// HubOption configures a Hub.
type HubOption func(*hubConfig) //nolint:revive

type hubConfig struct {
	connect            ConnectFunc
	onConnect          func(Connection)
	onMessage          func(Connection, Message)
	onDisconnect       func(Connection, error)
	onTransportDrop    func(Connection, error)
	onTransportRestore func(Connection)
	pingInterval       time.Duration
	writeTimeout       time.Duration
	maxMessageSize     int64
	sendBufferSize     int
	resumeWindow       time.Duration // session resume grace period as a time.Duration (e.g. 5*time.Minute); 0 = disabled
	codec              Codec
	logger             *zap.Logger
	clock              clock
	metrics            MetricsCollector
}

func defaultConfig(connect ConnectFunc) *hubConfig {
	return &hubConfig{
		connect:        connect,
		pingInterval:   20 * time.Second,
		writeTimeout:   10 * time.Second,
		maxMessageSize: 512,
		sendBufferSize: 256,
		resumeWindow:   0,
		codec:          JSONCodec,
		logger:         zap.NewNop(),
		clock:          realClock{},
		metrics:        NoopCollector{},
	}
}

// WithOnConnect registers a callback invoked after a connection is established
// and registered with the Hub. The callback runs in a separate goroutine.
func WithOnConnect(fn func(Connection)) HubOption {
	return func(c *hubConfig) { c.onConnect = fn }
}

// WithOnMessage registers a callback invoked for every inbound Message received from
// a connected client. The callback is called from the connection's readPump goroutine
// and must return quickly; use a goroutine for heavy work.
//
// NOTE: fn is always called from a single readPump goroutine per Connection.
// On resume, the new readPump starts only after the old one has fully exited.
// Handlers should still be safe for concurrent use when application code
// accesses Connection from other goroutines (e.g. Send from an HTTP handler).
func WithOnMessage(fn func(Connection, Message)) HubOption {
	return func(c *hubConfig) { c.onMessage = fn }
}

// WithOnDisconnect registers a callback invoked when a connection terminates.
// err is nil for a normal closure. The callback runs in a separate goroutine.
// When WithResumeWindow is configured, this fires only after the resume window
// expires without reconnection (not on every transport drop).
func WithOnDisconnect(fn func(Connection, error)) HubOption {
	return func(c *hubConfig) { c.onDisconnect = fn }
}

// WithOnTransportDrop registers a callback invoked when a connection's
// underlying WebSocket transport dies (network drop, read timeout, or peer
// close) and the session enters the suspended state because resumeWindow > 0.
//
// The error parameter carries the cause of the transport failure when available
// (e.g. an i/o timeout from a missed Pong, or a close frame from the peer).
// For a normal or expected close, err may be nil, so callback implementations
// must not assume it is always non-nil.
//
// This callback does NOT fire when:
//   - resumeWindow is 0 (OnDisconnect fires directly instead).
//   - the connection is removed via Kick() or Connection.Close()
//     (OnDisconnect fires directly instead).
//
// The callback runs in a separate goroutine; it must be safe for concurrent use.
func WithOnTransportDrop(fn func(Connection, error)) HubOption {
	return func(c *hubConfig) { c.onTransportDrop = fn }
}

// WithOnTransportRestore registers a callback invoked when a suspended session
// resumes after a client reconnects with the same connectionID within the
// resume window.
//
// When this fires, OnConnect and OnDisconnect are NOT called — the session
// continues as if the transport had never dropped. Buffered frames are replayed
// to the new transport before the callback is invoked.
//
// This callback does NOT fire when:
//   - resumeWindow is 0 (session resumption is disabled).
//   - the resume window expires before the client reconnects
//     (OnDisconnect fires instead).
//
// The callback runs in a separate goroutine; it must be safe for concurrent use.
func WithOnTransportRestore(fn func(Connection)) HubOption {
	return func(c *hubConfig) { c.onTransportRestore = fn }
}

// WithPingInterval sets the interval between heartbeat pings sent by the
// hub's pingPump goroutine. Each ping uses a synchronous Ping(ctx) call with
// a timeout equal to writeTimeout (configured via WithWriteTimeout). If the
// pong does not arrive within that timeout, the connection is considered dead.
// d must be in (writeTimeout, 1m]. Default: 20 s.
// NewHub panics if pingInterval <= writeTimeout after all options are applied.
func WithPingInterval(d time.Duration) HubOption {
	if d <= 0 {
		panic("wspulse: WithPingInterval: duration must be positive")
	}
	if d > maxPingInterval {
		panic(fmt.Sprintf("wspulse: WithPingInterval: duration exceeds maximum (%v)", maxPingInterval))
	}
	return func(c *hubConfig) { c.pingInterval = d }
}

// WithWriteTimeout sets the timeout for a single write operation on a
// connection, including Ping. d must be in (0, 30s]. Default: 10 s.
// NewHub panics if writeTimeout >= pingInterval after all options are applied.
func WithWriteTimeout(d time.Duration) HubOption {
	if d <= 0 {
		panic("wspulse: WithWriteTimeout: duration must be positive")
	}
	if d > maxWriteTimeout {
		panic(fmt.Sprintf("wspulse: WithWriteTimeout: duration exceeds maximum (%v)", maxWriteTimeout))
	}
	return func(c *hubConfig) { c.writeTimeout = d }
}

// WithMaxMessageSize sets the maximum size in bytes for inbound messages.
// n must be in [1, 67108864] (64 MiB).
func WithMaxMessageSize(n int64) HubOption {
	if n < 1 {
		panic("wspulse: WithMaxMessageSize: n must be at least 1")
	}
	if n > maxMsgSizeBytes {
		panic(fmt.Sprintf("wspulse: WithMaxMessageSize: n exceeds maximum (%d)", maxMsgSizeBytes))
	}
	return func(c *hubConfig) { c.maxMessageSize = n }
}

// WithSendBufferSize sets the per-connection outbound channel capacity (number of frames).
// n must be in [1, 4096].
func WithSendBufferSize(n int) HubOption {
	if n < 1 {
		panic("wspulse: WithSendBufferSize: n must be at least 1")
	}
	if n > maxSendBufFrames {
		panic(fmt.Sprintf("wspulse: WithSendBufferSize: n exceeds maximum (%d)", maxSendBufFrames))
	}
	return func(c *hubConfig) { c.sendBufferSize = n }
}

// WithCodec replaces the default JSONCodec with the provided Codec.
// Panics if codec is nil.
func WithCodec(codec Codec) HubOption {
	if codec == nil {
		panic("wspulse: WithCodec: codec must not be nil")
	}
	return func(c *hubConfig) { c.codec = codec }
}

// WithLogger sets the zap logger used for internal diagnostics.
// Defaults to zap.NewNop() (silent). Pass the application logger to route
// wspulse transport logs through the same zap core (encoder, level, async writer).
// Panics if l is nil; pass zap.NewNop() explicitly if a no-op logger is desired.
func WithLogger(l *zap.Logger) HubOption {
	if l == nil {
		panic("wspulse: WithLogger: logger must not be nil")
	}
	return func(c *hubConfig) { c.logger = l }
}

// WithResumeWindow configures the session resumption window. When a transport
// drops, the session is suspended for d before firing OnDisconnect. If the
// same connectionID reconnects within that period, the session resumes
// transparently.
// Valid range: 0 (disabled) … no upper limit. Default is 0 (OnDisconnect fires immediately).
func WithResumeWindow(d time.Duration) HubOption {
	if d < 0 {
		panic("wspulse: WithResumeWindow: duration must be non-negative")
	}
	return func(c *hubConfig) { c.resumeWindow = d }
}

// WithMetrics configures the MetricsCollector used by the Hub.
// Defaults to NoopCollector{} if not set.
// Panics if collector is nil.
func WithMetrics(collector MetricsCollector) HubOption {
	if collector == nil {
		panic("wspulse: WithMetrics: collector must not be nil")
	}
	return func(c *hubConfig) { c.metrics = collector }
}
