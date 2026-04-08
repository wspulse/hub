package wspulse

import core "github.com/wspulse/core"

// Clock exports the internal clock interface for testing only.
type Clock = clock

// WithClock returns a ServerOption that sets the clock. Test-only —
// this file is only compiled during test builds.
func WithClock(c Clock) ServerOption {
	if c == nil {
		panic("wspulse: WithClock: clock must not be nil")
	}
	return func(cfg *serverConfig) { cfg.clock = c }
}

// InjectTransport bypasses ServeHTTP and pushes a registerMessage directly
// into the hub's register channel. Test-only — allows component tests to
// inject mock transports without HTTP upgrade.
func InjectTransport(srv Server, connectionID, roomID string, transport core.Transport) {
	if srv == nil {
		panic("wspulse: InjectTransport: server must not be nil; use a server created by NewServer")
	}
	s, ok := srv.(*internalServer)
	if !ok {
		panic("wspulse: InjectTransport: only works with servers created by NewServer")
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
	case s.hub.register <- msg:
	case <-s.hub.done:
		panic("wspulse: InjectTransport: hub is stopped; cannot inject transport")
	}
}
