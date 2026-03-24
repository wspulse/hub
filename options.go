package wspulse

import (
	"net/http"
	"time"

	"go.uber.org/zap"
)

// Configuration upper bounds — option functions panic if these ceilings are exceeded.
const (
	maxPingPeriod    = 5 * time.Minute  // WithHeartbeat: pingPeriod upper bound
	maxPongWait      = 10 * time.Minute // WithHeartbeat: pongWait upper bound
	maxWriteWait     = 30 * time.Second // WithWriteWait upper bound
	maxMsgSizeBytes  = 64 << 20         // WithMaxMessageSize upper bound — 64 MiB
	maxSendBufFrames = 4096             // WithSendBufferSize upper bound
)

// ConnectFunc authenticates an incoming HTTP upgrade request and provides the
// roomID and connectionID for the new connection.
// Returning a non-nil error rejects the upgrade with HTTP 401 and the error text as the body.
// If connectionID is empty the Server assigns a random UUID so that every connection
// has a unique, non-empty ID. Use a non-empty connectionID when the application needs
// deterministic IDs (e.g. for Server.Send and Server.Kick).
type ConnectFunc func(r *http.Request) (roomID, connectionID string, err error)

// ServerOption configures a Server.
type ServerOption func(*serverConfig) //nolint:revive

type serverConfig struct {
	connect            ConnectFunc
	onConnect          func(Connection)
	onMessage          func(Connection, Frame)
	onDisconnect       func(Connection, error)
	onTransportDrop    func(Connection, error)
	onTransportRestore func(Connection)
	pingPeriod         time.Duration
	pongWait           time.Duration
	writeWait          time.Duration
	maxMessageSize     int64
	sendBufferSize     int
	resumeWindow       time.Duration // session resume grace period as a time.Duration (e.g. 5*time.Minute); 0 = disabled
	codec              Codec
	checkOrigin        func(r *http.Request) bool
	logger             *zap.Logger
	clock              clock
	metrics            MetricsCollector
}

func defaultConfig(connect ConnectFunc) *serverConfig {
	return &serverConfig{
		connect:        connect,
		pingPeriod:     10 * time.Second,
		pongWait:       30 * time.Second,
		writeWait:      10 * time.Second,
		maxMessageSize: 512,
		sendBufferSize: 256,
		resumeWindow:   0,
		codec:          JSONCodec,
		checkOrigin:    func(*http.Request) bool { return true },
		logger:         zap.NewNop(),
		clock:          realClock{},
		metrics:        NoopCollector{},
	}
}

// WithOnConnect registers a callback invoked after a connection is established
// and registered with the Server. The callback runs in a separate goroutine.
func WithOnConnect(fn func(Connection)) ServerOption {
	return func(c *serverConfig) { c.onConnect = fn }
}

// WithOnMessage registers a callback invoked for every inbound Frame received from
// a connected client. The callback is called from the connection's readPump goroutine
// and must return quickly; use a goroutine for heavy work.
//
// NOTE: fn is always called from a single readPump goroutine per Connection.
// On resume, the new readPump starts only after the old one has fully exited.
// Handlers should still be safe for concurrent use when application code
// accesses Connection from other goroutines (e.g. Send from an HTTP handler).
func WithOnMessage(fn func(Connection, Frame)) ServerOption {
	return func(c *serverConfig) { c.onMessage = fn }
}

// WithOnDisconnect registers a callback invoked when a connection terminates.
// err is nil for a normal closure. The callback runs in a separate goroutine.
// When WithResumeWindow is configured, this fires only after the resume window
// expires without reconnection (not on every transport drop).
func WithOnDisconnect(fn func(Connection, error)) ServerOption {
	return func(c *serverConfig) { c.onDisconnect = fn }
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
func WithOnTransportDrop(fn func(Connection, error)) ServerOption {
	return func(c *serverConfig) { c.onTransportDrop = fn }
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
func WithOnTransportRestore(fn func(Connection)) ServerOption {
	return func(c *serverConfig) { c.onTransportRestore = fn }
}

// WithHeartbeat configures Ping/Pong heartbeat intervals.
// Defaults: pingPeriod=10 s, pongWait=30 s.
// pingPeriod must be in (0, 5m] and pongWait must be in (pingPeriod, 10m].
func WithHeartbeat(pingPeriod, pongWait time.Duration) ServerOption {
	if pingPeriod <= 0 || pongWait <= 0 || pingPeriod >= pongWait {
		panic("wspulse: WithHeartbeat: pingPeriod must be positive and strictly less than pongWait")
	}
	if pingPeriod > maxPingPeriod {
		panic("wspulse: WithHeartbeat: pingPeriod exceeds maximum (5m)")
	}
	if pongWait > maxPongWait {
		panic("wspulse: WithHeartbeat: pongWait exceeds maximum (10m)")
	}
	return func(c *serverConfig) {
		c.pingPeriod = pingPeriod
		c.pongWait = pongWait
	}
}

// WithWriteWait sets the deadline for a single write operation on a connection.
// d must be in (0, 30s].
func WithWriteWait(d time.Duration) ServerOption {
	if d <= 0 {
		panic("wspulse: WithWriteWait: duration must be positive")
	}
	if d > maxWriteWait {
		panic("wspulse: WithWriteWait: duration exceeds maximum (30s)")
	}
	return func(c *serverConfig) { c.writeWait = d }
}

// WithMaxMessageSize sets the maximum size in bytes for inbound messages.
// n must be in [1, 67108864] (64 MiB).
func WithMaxMessageSize(n int64) ServerOption {
	if n < 1 {
		panic("wspulse: WithMaxMessageSize: n must be at least 1")
	}
	if n > maxMsgSizeBytes {
		panic("wspulse: WithMaxMessageSize: n exceeds maximum (64 MiB)")
	}
	return func(c *serverConfig) { c.maxMessageSize = n }
}

// WithSendBufferSize sets the per-connection outbound channel capacity (number of frames).
// n must be in [1, 4096].
func WithSendBufferSize(n int) ServerOption {
	if n < 1 {
		panic("wspulse: WithSendBufferSize: n must be at least 1")
	}
	if n > maxSendBufFrames {
		panic("wspulse: WithSendBufferSize: n exceeds maximum (4096)")
	}
	return func(c *serverConfig) { c.sendBufferSize = n }
}

// WithCodec replaces the default JSONCodec with the provided Codec.
// Panics if codec is nil.
func WithCodec(codec Codec) ServerOption {
	if codec == nil {
		panic("wspulse: WithCodec: codec must not be nil")
	}
	return func(c *serverConfig) { c.codec = codec }
}

// WithCheckOrigin sets the origin validation function for WebSocket upgrades.
// Defaults to accepting all origins (permissive — tighten this in production).
// Panics if fn is nil; pass the default (accept-all) explicitly if desired:
//
//	server.WithCheckOrigin(func(*http.Request) bool { return true })
func WithCheckOrigin(fn func(r *http.Request) bool) ServerOption {
	if fn == nil {
		panic("wspulse: WithCheckOrigin: fn must not be nil")
	}
	return func(c *serverConfig) { c.checkOrigin = fn }
}

// WithLogger sets the zap logger used for internal diagnostics.
// Defaults to zap.NewNop() (silent). Pass the application logger to route
// wspulse transport logs through the same zap core (encoder, level, async writer).
// Panics if l is nil; pass zap.NewNop() explicitly if a no-op logger is desired.
func WithLogger(l *zap.Logger) ServerOption {
	if l == nil {
		panic("wspulse: WithLogger: logger must not be nil")
	}
	return func(c *serverConfig) { c.logger = l }
}

// WithResumeWindow configures the session resumption window. When a transport
// drops, the session is suspended for d before firing OnDisconnect. If the
// same connectionID reconnects within that period, the session resumes
// transparently.
// Valid range: 0 (disabled) … no upper limit. Default is 0 (OnDisconnect fires immediately).
func WithResumeWindow(d time.Duration) ServerOption {
	if d < 0 {
		panic("wspulse: WithResumeWindow: duration must be non-negative")
	}
	return func(c *serverConfig) { c.resumeWindow = d }
}

// WithMetrics configures the MetricsCollector used by the Server.
// Defaults to NoopCollector{} if not set.
// Panics if collector is nil.
func WithMetrics(collector MetricsCollector) ServerOption {
	if collector == nil {
		panic("wspulse: WithMetrics: collector must not be nil")
	}
	return func(c *serverConfig) { c.metrics = collector }
}
