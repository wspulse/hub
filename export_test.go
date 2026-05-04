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
// The session's mockTransport must have BlockClose set before calling
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

// PrefillResumeBuffer pushes count copies of data into the named session's
// resume buffer via direct ForcePush, bypassing codec encoding and the
// suspended-state check. Test-only — used by BenchmarkResumeBufferDrain to
// stage a buffer for repeated drain measurements without paying encode cost
// per iteration.
//
// The session's resumeBuffer must be non-nil (i.e. the hub was created
// with WithResumeWindow > 0). Returns an error if the connection is not
// found or the session has no resume buffer.
func PrefillResumeBuffer(h Hub, connectionID string, data []byte, count int) error {
	if h == nil {
		panic("wspulse: PrefillResumeBuffer: hub must not be nil")
	}
	hub, ok := h.(*internalHub)
	if !ok {
		panic("wspulse: PrefillResumeBuffer: only works with hubs created by NewHub")
	}
	hub.heart.mu.RLock()
	sess, found := hub.heart.connectionsByID[connectionID]
	hub.heart.mu.RUnlock()
	if !found {
		return fmt.Errorf("wspulse: PrefillResumeBuffer: connection %q not found", connectionID)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.resumeBuffer == nil {
		return fmt.Errorf("wspulse: PrefillResumeBuffer: session %q has no resume buffer", connectionID)
	}
	for i := 0; i < count; i++ {
		sess.resumeBuffer.ForcePush(data)
	}
	return nil
}

// DrainResumeBuffer drains the named session's resume buffer back into its
// send queue, mirroring the drain step that attachWS runs in its transition
// goroutine. Returns the number of items drained. Test-only — paired with
// PrefillResumeBuffer to bench the drain operation in isolation from the
// transition-goroutine scheduling and InjectTransport routing overhead.
//
// Unlike attachWS, this helper does NOT flip the session state, so the
// caller can re-prefill and re-drain repeatedly inside a benchmark loop.
//
// DRIFT WARNING: this is a hand-rolled replica of the drain loop inside
// session.attachWS — they intentionally share the same locking pattern
// (sess.mu held around each resumeBuffer.Drain, released for per-message
// ForceEnqueue, re-acquired before the next iteration). If you change the
// prod drain logic (additional metrics, different lock scope, batched
// ForceEnqueue, etc.) you MUST update this helper to match, otherwise
// BenchmarkResumeBufferDrain will silently measure stale semantics and
// regression detection will misfire. The cleaner long-term fix is to
// extract drainResumeBuffer into a session method and have both attachWS
// and this helper call it (deferred — would need a callback parameter to
// preserve the atomic empty-Drain + state-flip invariant in attachWS).
func DrainResumeBuffer(h Hub, connectionID string) (int, error) {
	if h == nil {
		panic("wspulse: DrainResumeBuffer: hub must not be nil")
	}
	hub, ok := h.(*internalHub)
	if !ok {
		panic("wspulse: DrainResumeBuffer: only works with hubs created by NewHub")
	}
	hub.heart.mu.RLock()
	sess, found := hub.heart.connectionsByID[connectionID]
	hub.heart.mu.RUnlock()
	if !found {
		return 0, fmt.Errorf("wspulse: DrainResumeBuffer: connection %q not found", connectionID)
	}
	// Mirror attachWS's drain loop locking exactly: hold sess.mu around
	// each buffer.Drain() to serialise with concurrent Send() ForcePushes,
	// release for the per-message ForceEnqueue (which is itself thread-safe),
	// then re-acquire to check whether more arrived.
	sess.mu.Lock()
	if sess.resumeBuffer == nil {
		sess.mu.Unlock()
		return 0, fmt.Errorf("wspulse: DrainResumeBuffer: session %q has no resume buffer", connectionID)
	}
	drained := 0
	for {
		messages := sess.resumeBuffer.Drain()
		if len(messages) == 0 {
			sess.mu.Unlock()
			break
		}
		sess.mu.Unlock()
		for _, data := range messages {
			if _, err := sess.send.ForceEnqueue(data); err != nil {
				return drained, fmt.Errorf("wspulse: DrainResumeBuffer: %w", err)
			}
			drained++
		}
		sess.mu.Lock()
	}
	return drained, nil
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
