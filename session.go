package wspulse

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/zap"

	"github.com/maxence2997/carousel"

	core "github.com/wspulse/core"
)

// Connection represents a logical WebSocket session managed by the Hub.
// The underlying physical WebSocket may be transparently swapped on reconnect
// when WithResumeWindow is configured. All exported methods are safe to call
// concurrently.
type Connection interface {
	// ID returns the unique connection identifier provided by ConnectFunc.
	ID() string

	// RoomID returns the room this connection belongs to, as provided by ConnectFunc.
	RoomID() string

	// Send enqueues m for delivery to the remote peer.
	// Returns ErrConnectionClosed or ErrSendBufferFull on failure.
	Send(m Message) error

	// Close initiates a graceful shutdown of the session.
	Close() error

	// Done returns a channel that is closed when the session is terminated.
	Done() <-chan struct{}
}

// sessionState tracks the lifecycle of a session.
type sessionState int

const (
	stateConnected sessionState = iota // active WebSocket, pumps running
	stateSuspended                     // transport dead, within resume window
	stateClosed                        // session terminated
)

// session is the unexported, concrete implementation of Connection.
//
// Architecture:
//   - session is the stable, long-lived object that the application layer holds.
//   - session.transport is the current physical WebSocket connection. It may be nil
//     when suspended (waiting for reconnect within the resume window).
//   - readPump, writePump, and pingPump are goroutines that operate on a specific
//     transport. They share a pumpCtx and are respawned on each reconnect.
//   - session.send is shared across reconnects — it persists for the session lifetime.
//
// Goroutine ownership:
//   - readPump  : reads from transport, forwards decoded Messages to onMessage.
//   - writePump : sole writer of application data on transport; drains session.send.
//   - pingPump  : drives Ping heartbeat; fires HeartbeatFailed metric on failure.
//
// Lifecycle signal flow:
//
//	close(session.done) → pumpCancel() via bridge goroutine → all pumps exit.
//	readPump sees a read error (from the closed transport) and sends transportDiedMessage.
type session struct {
	id     string
	roomID string
	send   *carousel.RingQueue[[]byte] // outbound message queue; shared across reconnects
	done   chan struct{}               // closed once to signal session termination; guarded by closeOnce

	mu           sync.Mutex                   // guards transport, pumpCancel, pumpDone, graceTimer, state, resumeBuffer, suspendEpoch, closeCode, closeReason
	transport    transport                    // current physical connection; nil when suspended
	pumpCancel   context.CancelFunc           // cancels the current pump context
	pumpDone     chan struct{}                // closed by writePump on exit
	graceTimer   *time.Timer                  // resume window timer; nil when not suspended
	state        sessionState                 // current lifecycle state
	resumeBuffer *carousel.RingBuffer[[]byte] // nil when resume is disabled
	suspendEpoch uint64                       // monotonically increases on each detachWS; stale grace timers compare this
	closeCode    core.StatusCode              // close-frame status code writePump emits on session shutdown; default StatusNormalClosure
	closeReason  string                       // close-frame reason string writePump emits on session shutdown; default ""

	connectedAt time.Time // session creation time; written once, read-only thereafter

	closeOnce sync.Once
	config    *hubConfig
}

func (s *session) ID() string            { return s.id }
func (s *session) RoomID() string        { return s.roomID }
func (s *session) Done() <-chan struct{} { return s.done }

// Send encodes m and enqueues the bytes for delivery to the remote peer.
// If the session is suspended (within resume window), the message is buffered
// to the resume ring buffer instead of the outbound send queue.
//
// The select is a fast-path optimisation: skip encoding when the session is
// already closed. If the session closes in the window between this check and
// enqueue, enqueue will return ErrConnectionClosed.
func (s *session) Send(m Message) error {
	// Fast path: bail early if the session is already closed.
	select {
	case <-s.done:
		return ErrConnectionClosed
	default:
	}

	data, err := s.config.codec.Encode(m)
	if err != nil {
		return err
	}

	return s.enqueue(data, false)
}

// enqueue sends pre-encoded data to the appropriate destination based on
// the session state. Used by both Send (after encoding) and the hub's
// broadcast path (which pre-encodes once for all connections).
//
// When dropOldest is true and the send buffer is full, the oldest message
// in the buffer is discarded to make room for data. This is the
// backpressure strategy used by Broadcast. When false, ErrSendBufferFull
// is returned immediately (the strategy used by Send).
func (s *session) enqueue(data []byte, dropOldest bool) error {
	// Check if we need to buffer (suspended state).
	s.mu.Lock()
	if s.state == stateSuspended && s.resumeBuffer != nil {
		dropped := s.resumeBuffer.ForcePush(data)
		s.mu.Unlock()
		if dropped {
			s.config.metrics.MessageDropped(s.roomID, s.id)
			s.config.logger.Debug("wspulse: oldest message dropped from resumeBuffer (backpressure)",
				zap.String("conn_id", s.id),
			)
		} else {
			s.config.logger.Debug("wspulse: message buffered to resumeBuffer",
				zap.String("conn_id", s.id),
			)
		}
		return nil
	}
	s.mu.Unlock()

	if dropOldest {
		evicted, err := s.send.ForceEnqueue(data)
		if errors.Is(err, carousel.ErrClosed) {
			return ErrConnectionClosed
		}
		if err != nil {
			return err
		}
		if evicted {
			s.config.logger.Debug("wspulse: oldest message dropped from send buffer (backpressure)",
				zap.String("conn_id", s.id),
			)
			s.config.metrics.MessageDropped(s.roomID, s.id)
		}
		return nil
	}

	err := s.send.Enqueue(data)
	if errors.Is(err, carousel.ErrFull) {
		s.config.logger.Debug("wspulse: send buffer full, dropping message",
			zap.String("conn_id", s.id),
		)
		s.config.metrics.MessageDropped(s.roomID, s.id)
		return ErrSendBufferFull
	}
	if errors.Is(err, carousel.ErrClosed) {
		return ErrConnectionClosed
	}
	return err
}

// cancelGraceTimer stops any running grace timer and bumps the suspend epoch
// so that in-flight graceExpiredMessages are detected as stale.
func (s *session) cancelGraceTimer() {
	s.mu.Lock()
	if s.graceTimer != nil {
		s.graceTimer.Stop()
		s.graceTimer = nil
	}
	s.suspendEpoch++
	s.mu.Unlock()
}

// Close initiates a graceful shutdown of the session. When a transport
// is attached, writePump emits a close frame with (StatusNormalClosure,
// "") to the remote peer before exiting. On a suspended session
// (transport already dropped, awaiting resume) no close frame is sent —
// the session is torn down via the heart's graceExpired path. This is
// the public Connection.Close() entry point used by application code.
//
// Heart-driven teardown paths use closeWith directly to encode the
// disconnect cause (e.g. kick, hub shutdown, duplicate conn_id) into the
// close frame's reason string when one is emitted.
//
// Safe to call multiple times; only the first call has effect.
func (s *session) Close() error {
	return s.closeWith(core.StatusNormalClosure, "")
}

// closeWith terminates the session and instructs writePump to emit a close
// frame carrying the supplied (code, reason). Setting the close-frame
// fields and closing s.done happen inside the same closeOnce.Do body, so
// writePump (which only reads these fields after observing s.done closed)
// is guaranteed to see them.
//
// Ordering note: heart-driven teardown (disconnectSession) always calls
// cancelGraceTimer via removeSession before calling closeWith. By the time
// closeWith acquires the lock, graceTimer is already nil, so timer.Reset(0)
// is never invoked on that path. timer.Reset(0) is only reached when the
// application calls Close() (which routes through closeWith) directly on a
// suspended session; in that case it is intentional — it signals the heart
// via the existing graceExpired channel without requiring a separate
// session-to-heart channel.
//
// Safe to call multiple times; only the first call has effect.
func (s *session) closeWith(code core.StatusCode, reason string) error {
	s.closeOnce.Do(func() {
		s.config.logger.Debug("wspulse: session closing",
			zap.String("conn_id", s.id),
			zap.Int("close_code", int(code)),
			zap.String("close_reason", reason),
		)
		s.mu.Lock()
		s.closeCode = code
		s.closeReason = reason
		s.state = stateClosed
		timer := s.graceTimer
		s.graceTimer = nil
		if s.resumeBuffer != nil {
			s.resumeBuffer = nil
		}
		s.mu.Unlock()

		if timer != nil {
			// Reset to 0 so handleGraceExpired fires immediately.
			// See ordering note on closeWith above.
			timer.Reset(0)
		}

		close(s.done)
		s.send.Close()
	})
	return nil
}

// attachWS sets the physical WebSocket connection for this session and
// spawns readPump + writePump + pingPump goroutines. If the session was
// suspended, buffered messages are drained into the send queue before the
// new pumps start.
//
// onResumeComplete, if non-nil, is invoked in a separate goroutine after
// the resume drain completes and all pumps have started. This ensures
// the callback fires only when the session is in stateConnected with
// active pumps. Pass nil for new (non-resume) sessions.
//
// The method returns immediately without blocking the caller (the hub's
// event loop). A transition goroutine waits for the old writePump to exit,
// drains the resume buffer, and then starts readPump, writePump, and pingPump.
// This avoids three problems:
//   - The heart event loop being blocked for up to writeTimeout while waiting
//     for the old writePump to finish.
//   - Resume-buffer messages being drained into s.send while the old
//     writePump is still alive, which could cause the old pump to consume
//     and lose those messages by writing them to the dead WebSocket.
//   - readPump reporting a transportDied while the session is still in
//     stateSuspended (during the drain phase), which would leave the
//     session in a zombie state — stateConnected with no active pumps.
//
// Message ordering guarantee: the state remains stateSuspended during the
// drain so that concurrent Send() calls continue buffering to resumeBuffer.
// The drain loop runs until resumeBuffer is empty, then atomically flips the
// state to stateConnected under the same lock acquisition. This ensures
// all pre-resume messages precede post-resume messages in s.send.
//
// Must be called from the heart's event loop (single-goroutine serialization).
func (s *session) attachWS(trans transport, h *heart, onResumeComplete func()) {
	s.mu.Lock()

	// Stop the previous pump group if still running.
	if s.pumpCancel != nil {
		s.pumpCancel()
	}
	oldPumpDone := s.pumpDone

	s.transport = trans
	pumpCtx, pumpCancel := context.WithCancel(context.Background())
	s.pumpCancel = pumpCancel
	s.pumpDone = make(chan struct{})

	// Keep the current state — for resume sessions this stays
	// stateSuspended until the transition goroutine finishes draining.
	// For new sessions the state is already stateConnected.
	isResume := s.state == stateSuspended
	pumpDone := s.pumpDone
	buffer := s.resumeBuffer
	s.mu.Unlock()

	// Bridge goroutine: propagate session termination to pump context.
	go func() {
		select {
		case <-s.done:
			pumpCancel()
		case <-pumpCtx.Done():
		}
	}()

	// Transition goroutine: wait for the old writePump to exit, drain
	// the resume buffer, and start the new pump group. This guarantees:
	// 1. Only one writePump drains s.send at a time.
	// 2. Resume-buffer messages enter s.send only after the old pump is gone.
	// 3. The heart event loop is never blocked.
	// 4. All buffered messages precede messages sent after the state flip.
	// 5. readPump only runs when state is stateConnected, preventing
	//    transportDied messages from arriving during stateSuspended.
	go func() {
		if oldPumpDone != nil {
			<-oldPumpDone
		}

		if isResume && buffer != nil {
			// Drain-and-flip loop: while state is stateSuspended,
			// concurrent Send() calls continue pushing to resumeBuffer.
			// Drain until empty, then atomically set stateConnected
			// under the same lock — no reordering is possible.
			s.mu.Lock()
			bufferedCount := buffer.Len()
			s.config.logger.Debug("wspulse: draining resumeBuffer",
				zap.String("conn_id", s.id),
				zap.Int("buffered", bufferedCount),
			)
			for {
				messages := buffer.Drain()
				if len(messages) == 0 {
					// Guard: if Close() was called concurrently, do not
					// overwrite stateClosed with stateConnected.
					if s.state != stateClosed {
						s.state = stateConnected
					} else {
						s.config.logger.Debug("wspulse: attachWS drain aborted — session closed mid-drain",
							zap.String("conn_id", s.id),
						)
					}
					s.mu.Unlock()
					break
				}
				s.mu.Unlock()
				for _, data := range messages {
					evicted, err := s.send.ForceEnqueue(data)
					if err != nil {
						// Queue closed — session terminated during drain.
						s.config.logger.Debug("wspulse: resume drain aborted — send queue closed",
							zap.String("conn_id", s.id),
						)
						break
					}
					if evicted {
						s.config.logger.Debug("wspulse: oldest message dropped to make room for resume message",
							zap.String("conn_id", s.id),
						)
						s.config.metrics.MessageDropped(s.roomID, s.id)
					} else {
						s.config.logger.Debug("wspulse: resume message enqueued",
							zap.String("conn_id", s.id),
						)
					}
				}
				s.mu.Lock()
			}
		}

		// Guard: if the transport died during the transition (handled by heart
		// setting s.transport = nil), or was replaced by another attachWS call,
		// do not start pumps on the stale/dead transport. Signal pumpDone so
		// future transitions don't block waiting for this pump.
		s.mu.Lock()
		if s.transport != trans {
			s.config.logger.Warn("wspulse: transition goroutine aborted — transport replaced or nil'd during drain",
				zap.String("conn_id", s.id),
			)
			s.mu.Unlock()
			// Close the orphaned transport — no pumps were started on it, so
			// writePump's defer will never close it. Without this, the
			// underlying TCP connection and file descriptor leak.
			_ = trans.CloseNow()
			pumpCancel()
			close(pumpDone)
			return
		}
		s.mu.Unlock()

		go s.readPump(pumpCtx, trans, h)
		go s.writePump(pumpCtx, trans, pumpDone)
		go s.pingPump(pumpCtx, trans)

		if onResumeComplete != nil {
			s.mu.Lock()
			shouldCall := s.state == stateConnected && s.transport == trans
			s.mu.Unlock()
			if shouldCall {
				go onResumeComplete()
			}
		}
	}()
}

// detachWS clears the physical WebSocket from the session and transitions
// to the suspended state. Returns the new suspendEpoch and true on success.
// Returns (0, false) if the session is already closed — callers must not
// set a grace timer in that case.
//
// Must be called from the heart's event loop.
func (s *session) detachWS() (epoch uint64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == stateClosed {
		return 0, false
	}

	// Stop the current pump group.
	if s.pumpCancel != nil {
		s.pumpCancel()
		s.pumpCancel = nil
	}
	s.transport = nil
	s.state = stateSuspended
	s.suspendEpoch++
	return s.suspendEpoch, true
}

// pingPump drives the heartbeat ping/pong mechanism on the transport.
// Sends a Ping at each tick of pingInterval and waits for the pong reply
// within writeTimeout. On failure, fires HeartbeatFailed metric and calls
// CloseNow() to force-close the transport (without cancelling the context,
// so other pumps detect the error via their own I/O failures).
func (s *session) pingPump(ctx context.Context, trans transport) {
	ticker := s.config.clock.NewTicker(s.config.pingInterval)
	defer ticker.Stop()

	// Send an initial ping immediately so dead-on-arrival connections are
	// detected within writeTimeout instead of waiting a full pingInterval.
	if !s.doPing(ctx, trans) {
		return
	}

	for {
		select {
		case <-ticker.C:
			if !s.doPing(ctx, trans) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// doPing sends a single Ping with a writeTimeout deadline. Returns true if the
// pong arrived successfully, false if the caller should exit.
func (s *session) doPing(ctx context.Context, trans transport) bool {
	pingCtx, cancel := context.WithTimeout(ctx, s.config.writeTimeout)
	err := trans.Ping(pingCtx)
	cancel()
	if err != nil {
		// context.Canceled means the pump context was cancelled (reconnect/close);
		// this is not a heartbeat failure — exit silently without firing the metric.
		if ctx.Err() != nil {
			return false
		}
		s.config.metrics.HeartbeatFailed(s.roomID, s.id)
		s.config.logger.Debug("wspulse: pingPump stopping: ping failed",
			zap.String("conn_id", s.id), zap.Error(err))
		_ = trans.CloseNow()
		return false
	}
	return true
}

// readPump reads inbound messages from the transport and forwards them to the OnMessage
// callback. When the read loop exits it signals the heart that this transport
// has died. If the heart is shutting down, cleanup is handled inline.
func (s *session) readPump(ctx context.Context, trans transport, h *heart) {
	var readErr error
	defer func() {
		// Recover from panics in OnMessage handlers.
		if r := recover(); r != nil {
			stack := debug.Stack()
			readErr = &PanicError{Value: r, Stack: stack}
			s.config.logger.Error("wspulse: readPump panic recovered",
				zap.String("conn_id", s.id),
				zap.Any("panic", r),
				zap.ByteString("stack", stack),
			)
		}

		// Notify the heart that this transport died.
		select {
		case h.transportDied <- transportDiedMessage{session: s, transport: trans, err: readErr}:
		case <-h.done:
			// Hub has stopped; clean up inline.
		}

		// Unconditionally close done if nothing else will process this.
		// If resume is enabled, the heart will handle state transition;
		// this is a safety net for the heart-shutdown path.
		select {
		case <-h.done:
			s.config.logger.Debug("wspulse: readPump closed done inline (heart shutdown)",
				zap.String("conn_id", s.id),
			)
			s.closeOnce.Do(func() { close(s.done) })
		default:
		}
	}()

	trans.SetReadLimit(s.config.maxMessageSize)

	for {
		_, data, err := trans.Read(ctx)
		if err != nil {
			if ctx.Err() != nil || isNormalClose(err) {
				s.config.logger.Debug("wspulse: connection closed normally",
					zap.String("conn_id", s.id),
				)
			} else {
				readErr = err
				s.config.logger.Warn("wspulse: unexpected close", zap.String("conn_id", s.id), zap.Error(err))
			}
			return
		}
		s.config.metrics.MessageReceived(s.roomID, len(data))
		if fn := s.config.onMessage; fn != nil {
			msg, decodeErr := s.config.codec.Decode(data)
			if decodeErr != nil {
				s.config.logger.Warn("wspulse: decode failed", zap.String("conn_id", s.id), zap.Error(decodeErr))
				continue
			}
			fn(s, msg)
		}
	}
}

// isNormalClose reports whether err represents an expected, orderly
// connection close. When true, OnDisconnect receives a nil error.
//
// Normal close conditions:
//   - context.Canceled: the pump context was cancelled (reconnect, close, kick).
//   - Close status 1000 (StatusNormalClosure): remote sent a standard close frame.
//   - Close status 1001 (StatusGoingAway): remote is shutting down (e.g. browser tab closed).
//
// Everything else is abnormal and propagated to OnDisconnect /
// OnTransportDrop as a non-nil error: ping timeout (CloseNow → net
// error), TCP drops (RST/FIN/EOF), protocol errors, and other read
// failures. Note: the error value only affects what the callback
// receives — the suspend-vs-disconnect decision is made by the heart
// based solely on resumeWindow, independent of the error.
func isNormalClose(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	code := websocket.CloseStatus(err)
	return code == websocket.StatusNormalClosure ||
		code == websocket.StatusGoingAway
}

// emitCloseFrameOnShutdown sends the configured close frame to trans when
// the session is shutting down (s.done closed). On reconnect-swap (ctx
// cancelled but s.done still open) this is a no-op — the replacement pump
// owns the connection.
//
// closeCode/closeReason are normally set by closeWith inside the same
// closeOnce.Do body that closes s.done; observing s.done closed gives the
// happens-before edge needed to read them. The readPump heart-shutdown
// safety-net path closes s.done directly via closeOnce.Do without going
// through closeWith — when that race wins, the fields keep the constructor
// defaults (StatusNormalClosure, "") set in handleRegister, which is still
// a valid close frame. The s.mu acquisition mirrors closeWith's write site
// for access-pattern consistency.
//
// All writePump exit paths that may run during shutdown call this helper so
// the close frame is emitted regardless of which I/O step the pump was on
// when shutdown fired.
func (s *session) emitCloseFrameOnShutdown(trans transport) {
	select {
	case <-s.done:
		s.mu.Lock()
		code, reason := s.closeCode, s.closeReason
		s.mu.Unlock()
		_ = trans.Close(code, reason)
	default:
	}
}

// writePump drains the send channel on the transport. writePump is the sole
// goroutine that writes application data to the transport. On exit it
// force-closes the underlying connection so that readPump's Read unblocks.
//
// pumpDone is closed on exit so callers can wait for this pump to finish.
func (s *session) writePump(ctx context.Context, trans transport, pumpDone chan struct{}) {
	defer func() {
		_ = trans.CloseNow()
		close(pumpDone)
	}()

	for {
		// Priority exit: ctx cancelled while session is still alive
		// (reconnect swap) — exit fast so the replacement pump owns the
		// remaining messages in s.send. On full session shutdown (s.done
		// closed) fall through so the err branch sends the graceful close
		// frame; closeWith closes both s.done and the send queue, so the
		// next Pop returns promptly with either ErrClosed or ctx.Err().
		select {
		case <-ctx.Done():
			select {
			case <-s.done:
				// Session shutting down — fall through to the standard
				// exit path so writePump emits the configured close frame.
			default:
				return
			}
		default:
		}

		data, err := s.send.Pop(ctx)
		if err != nil {
			if errors.Is(err, carousel.ErrClosed) {
				s.config.logger.Debug("wspulse: writePump stopping: send queue closed",
					zap.String("conn_id", s.id))
			} else {
				s.config.logger.Debug("wspulse: writePump stopping: context cancelled",
					zap.String("conn_id", s.id))
			}
			s.emitCloseFrameOnShutdown(trans)
			return
		}

		writeCtx, cancel := context.WithTimeout(ctx, s.config.writeTimeout)
		err = trans.Write(writeCtx, s.config.codec.WireType(), data)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				s.config.logger.Debug("wspulse: writePump stopping: context cancelled",
					zap.String("conn_id", s.id))
				// Mirror the Pop-err branch above: shutdown can cancel
				// pumpCtx while we are inside trans.Write (real TCP writes
				// can block for up to writeTimeout). Without this, hub
				// shutdown silently drops the close frame whenever
				// writePump happens to be mid-write.
				s.emitCloseFrameOnShutdown(trans)
				return
			}
			s.config.logger.Warn("wspulse: write failed", zap.String("conn_id", s.id), zap.Error(err))
			return
		}
		s.config.metrics.MessageSent(s.roomID, s.id, len(data))
		s.config.metrics.SendBufferUtilization(s.roomID, s.id, s.send.Len(), s.send.Cap())
	}
}
