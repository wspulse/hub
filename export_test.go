package wspulse

import (
	"net/http"
	"time"
)

// DefaultPingInterval returns the default pingInterval from defaultConfig.
// Exposed as a function (not a var) so external test packages cannot mutate it.
func DefaultPingInterval() time.Duration {
	return defaultConfig(func(*http.Request) (string, string, error) { return "", "", nil }).pingInterval
}

// DefaultWriteTimeout returns the default writeTimeout from defaultConfig.
// Exposed as a function (not a var) so external test packages cannot mutate it.
func DefaultWriteTimeout() time.Duration {
	return defaultConfig(func(*http.Request) (string, string, error) { return "", "", nil }).writeTimeout
}

// Clock exports the internal clock interface for testing only.
type Clock = clock

// Transport exports the internal transport interface for testing only.
// A type alias (=) is used so that mock implementations in external _test
// packages automatically satisfy the unexported transport interface without
// an explicit cast. Same pattern as Clock = clock above.
type Transport = transport

// WithClock returns a HubOption that sets the clock. Test-only —
// this file is only compiled during test builds.
func WithClock(c Clock) HubOption {
	if c == nil {
		panic("wspulse: WithClock: clock must not be nil")
	}
	return func(cfg *hubConfig) { cfg.clock = c }
}

// InjectTransport bypasses ServeHTTP and pushes a registerMessage directly
// into the Hub's internal event loop. Test-only — allows component tests to
// inject mock transports without HTTP upgrade.
func InjectTransport(h Hub, connectionID, roomID string, transport Transport) {
	if h == nil {
		panic("wspulse: InjectTransport: hub must not be nil; use a hub created by NewHub")
	}
	s, ok := h.(*internalHub)
	if !ok {
		panic("wspulse: InjectTransport: only works with hubs created by NewHub")
	}
	if transport == nil {
		panic("wspulse: InjectTransport: transport must not be nil")
	}
	msg := registerMessage{
		connectionID: connectionID,
		roomID:       roomID,
		transport:    transport,
	}
	select {
	case s.heart.register <- msg:
	case <-s.heart.done:
		panic("wspulse: InjectTransport: heart is stopped; cannot inject transport")
	}
}
