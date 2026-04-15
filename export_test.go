package wspulse

import (
	"net/http"

	core "github.com/wspulse/core"
)

// DefaultPingInterval and DefaultWriteTimeout expose the default configuration
// values so external test packages can verify the constraint pingInterval > writeTimeout.
var (
	DefaultPingInterval = defaultConfig(func(*http.Request) (string, string, error) { return "", "", nil }).pingInterval
	DefaultWriteTimeout = defaultConfig(func(*http.Request) (string, string, error) { return "", "", nil }).writeTimeout
)

// Clock exports the internal clock interface for testing only.
type Clock = clock

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
func InjectTransport(h Hub, connectionID, roomID string, transport core.Transport) {
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
