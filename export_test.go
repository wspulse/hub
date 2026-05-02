package wspulse

import (
	"fmt"
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

// CloseSessionSendQueue closes the named session's outbound carousel queue
// without closing its done channel. Test-only seam used to deterministically
// reproduce the race in handleBroadcast where a session closes mid-broadcast:
// the soft `<-target.done` check falls through to default while
// `target.enqueue` returns carousel.ErrClosed. See issue wspulse/hub#63.
//
// The session's mockTransport must have BlockCloseNow set before calling
// this helper, otherwise the writePump's deferred CloseNow will tear the
// session down through the standard transportDied path before the test can
// observe the broadcast.
func CloseSessionSendQueue(h Hub, connectionID string) error {
	if h == nil {
		panic("wspulse: CloseSessionSendQueue: hub must not be nil")
	}
	hub, ok := h.(*internalHub)
	if !ok {
		panic("wspulse: CloseSessionSendQueue: only works with hubs created by NewHub")
	}
	hub.heart.mu.RLock()
	sess, found := hub.heart.connectionsByID[connectionID]
	hub.heart.mu.RUnlock()
	if !found {
		return fmt.Errorf("wspulse: CloseSessionSendQueue: connection %q not found", connectionID)
	}
	sess.send.Close()
	return nil
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
