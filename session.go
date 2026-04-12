package wspulse

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/zap"

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

	// Send enqueues f for delivery to the remote peer.
	// Returns ErrConnectionClosed or ErrSendBufferFull on failure.
	Send(f Frame) error

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
//   - readPump  : reads from transport, forwards decoded Frames to onMessage.
//   - writePump : sole writer on transport; drains session.send.
//   - pingPump  : drives Ping heartbeat; fires PongTimeout metric on failure.
//
// Lifecycle signal flow:
//
//	close(session.done) → pumpCancel() via bridge goroutine → all pumps exit.
//	readPump sees a read error (from the closed transport) and sends transportDiedMessage.
type session struct {
	id     string
	roomID string
	send   chan []byte   // raw encoded frames; never closed, shared across reconnects
	done   chan struct{} // closed once to signal session termination; guarded by closeOnce

	mu           sync.Mutex         // guards transport, pumpCancel, pumpDone, graceTimer, state, resumeBuffer, suspendEpoch
	transport    core.Transport     // current physical connection; nil when suspended
	pumpCancel   context.CancelFunc // cancels the current pump context
	pumpDone     chan struct{}      // closed by writePump on exit
	graceTimer   *time.Timer        // resume window timer; nil when not suspended
	state        sessionState       // current lifecycle state
	resumeBuffer *ringBuffer        // nil when resume is disabled
	suspendEpoch uint64             // monotonically increases on each detachWS; stale grace timers compare this

	connectedAt time.Time // session creation time; written once, read-only thereafter

	closeOnce sync.Once
	config    *hubConfig
}

func (s *session) ID() string            { return s.id }
func (s *session) RoomID() string        { return s.roomID }
func (s *session) Done() <-chan struct{} { return s.done }

// Send encodes f and enqueues the bytes for delivery to the remote peer.
// If the session is suspended (within resume window), the frame is buffered
// to the resume ring buffer instead of the send channel.
//
// The first select is a fast-path optimisation: skip encoding when the
// session is already closed. The second select is the authoritative check.
func (s *session) Send(f Frame) error {
	// Fast path: bail early if the session is already closed.
	select {
	case <-s.done:
		return ErrConnectionClosed
	default:
	}

	data, err := s.config.codec.Encode(f)
	if err != nil {
		return err
	}

	return s.enqueue(data, false)
}

// enqueue sends pre-encoded data to the appropriate destination based on
// the session state. Used by both Send (after encoding) and the hub's
// broadcast path (which pre-encodes once for all connections).
//
// When dropOldest is true and the send buffer is full, the oldest frame
// in the buffer is discarded to make room for data. This is the
// backpressure strategy used by Broadcast. When false, ErrSendBufferFull
// is returned immediately (the strategy used by Send).
func (s *session) enqueue(data []byte, dropOldest bool) error {
	// Check if we need to buffer (suspended state).
	s.mu.Lock()
	if s.state == stateSuspended && s.resumeBuffer != nil {
		dropped := s.resumeBuffer.Push(data)
		s.mu.Unlock()
		if dropped {
			s.config.metrics.FrameDropped(s.roomID, s.id)
			s.config.logger.Debug("wspulse: oldest frame dropped from resumeBuffer (backpressure)",
				zap.String("conn_id", s.id),
			)
		} else {
			s.config.logger.Debug("wspulse: frame buffered to resumeBuffer",
				zap.String("conn_id", s.id),
			)
		}
		return nil
	}
	s.mu.Unlock()

	// Authoritative check: three-way select evaluates all outcomes atomically.
	select {
	case s.send <- data:
		return nil
	case <-s.done:
		return ErrConnectionClosed
	default:
		if !dropOldest {
			s.config.logger.Debug("wspulse: send buffer full, dropping frame",
				zap.String("conn_id", s.id),
			)
			s.config.metrics.FrameDropped(s.roomID, s.id)
			return ErrSendBufferFull
		}
	}

	// Drop-oldest backpressure: discard the oldest frame and retry.
	select {
	case <-s.send:
		s.config.logger.Debug("wspulse: oldest frame dropped from send buffer (backpressure)",
			zap.String("conn_id", s.id),
		)
		s.config.metrics.FrameDropped(s.roomID, s.id)
	default:
	}
	select {
	case s.send <- data:
		return nil
	default:
		s.config.logger.Debug("wspulse: frame irrecoverably dropped: send buffer still full after drop-oldest",
			zap.String("conn_id", s.id),
		)
		s.config.metrics.FrameDropped(s.roomID, s.id)
		return ErrSendBufferFull
	}
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

// Close initiates a graceful shutdown of the session.
// Signals writePump to send a WebSocket close frame and stop.
// Safe to call multiple times; only the first call has effect.
//
// Ordering note: heart-driven teardown (disconnectSession) always calls
// cancelGraceTimer via removeSession before calling Close(). By the time
// Close() acquires the lock, graceTimer is already nil, so timer.Reset(0)
// is never invoked on that path. timer.Reset(0) is only reached when the
// application calls Close() directly on a suspended session; in that case
// it is intentional — it signals the heart via the existing graceExpired
// channel without requiring a separate session-to-heart channel.
func (s *session) Close() error {
	s.closeOnce.Do(func() {
		s.config.logger.Debug("wspulse: session closing",
			zap.String("conn_id", s.id),
		)
		s.mu.Lock()
		s.state = stateClosed
		timer := s.graceTimer
		s.graceTimer = nil
		if s.resumeBuffer != nil {
			s.resumeBuffer = nil
		}
		s.mu.Unlock()

		if timer != nil {
			// Reset to 0 so handleGraceExpired fires immediately.
			// See ordering note on Close() above.
			timer.Reset(0)
		}

		close(s.done)
	})
	return nil
}

// attachWS sets the physical WebSocket connection for this session and
// spawns readPump + writePump + pingPump goroutines. If the session was
// suspended, buffered frames are drained into the send channel before the
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
//   - Resume-buffer frames being drained into s.send while the old
//     writePump is still alive, which could cause the old pump to consume
//     and lose those frames by writing them to the dead WebSocket.
//   - readPump reporting a transportDied while the session is still in
//     stateSuspended (during the drain phase), which would leave the
//     session in a zombie state — stateConnected with no active pumps.
//
// Frame ordering guarantee: the state remains stateSuspended during the
// drain so that concurrent Send() calls continue buffering to resumeBuffer.
// The drain loop runs until resumeBuffer is empty, then atomically flips the
// state to stateConnected under the same lock acquisition. This ensures
// all pre-resume frames precede post-resume frames in s.send.
//
// Must be called from the heart's event loop (single-goroutine serialization).
func (s *session) attachWS(transport core.Transport, h *heart, onResumeComplete func()) {
	s.mu.Lock()

	// Stop the previous pump group if still running.
	if s.pumpCancel != nil {
		s.pumpCancel()
	}
	oldPumpDone := s.pumpDone

	s.transport = transport
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
	// 2. Resume-buffer frames enter s.send only after the old pump is gone.
	// 3. The heart event loop is never blocked.
	// 4. All buffered frames precede frames sent after the state flip.
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
				frames := buffer.Drain()
				if len(frames) == 0 {
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
				for _, data := range frames {
					select {
					case s.send <- data:
						s.config.logger.Debug("wspulse: resume frame enqueued",
							zap.String("conn_id", s.id),
						)
					default:
						// Send buffer full — apply drop-oldest to make room.
						s.config.logger.Debug("wspulse: send buffer full during resume drain, applying drop-oldest",
							zap.String("conn_id", s.id),
						)
						select {
						case <-s.send:
							s.config.logger.Debug("wspulse: oldest frame dropped to make room for resume frame",
								zap.String("conn_id", s.id),
							)
							s.config.metrics.FrameDropped(s.roomID, s.id)
						default:
						}
						select {
						case s.send <- data:
							s.config.logger.Debug("wspulse: resume frame enqueued after drop-oldest",
								zap.String("conn_id", s.id),
							)
						default:
							s.config.logger.Warn("wspulse: resume frame dropped: send buffer still full after drop-oldest",
								zap.String("conn_id", s.id),
							)
							s.config.metrics.FrameDropped(s.roomID, s.id)
						}
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
		if s.transport != transport {
			s.config.logger.Warn("wspulse: transition goroutine aborted — transport replaced or nil'd during drain",
				zap.String("conn_id", s.id),
			)
			s.mu.Unlock()
			// Close the orphaned transport — no pumps were started on it, so
			// writePump's defer will never close it. Without this, the
			// underlying TCP connection and file descriptor leak.
			_ = transport.CloseNow()
			close(pumpDone)
			return
		}
		s.mu.Unlock()

		go s.readPump(pumpCtx, transport, h)
		go s.writePump(pumpCtx, transport, pumpDone)
		go s.pingPump(pumpCtx, transport)

		if onResumeComplete != nil {
			s.mu.Lock()
			shouldCall := s.state == stateConnected && s.transport == transport
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
// within writeTimeout. On failure, fires PongTimeout metric and calls
// CloseNow() to force-close the transport (without cancelling the context,
// so other pumps detect the error via their own I/O failures).
func (s *session) pingPump(ctx context.Context, transport core.Transport) {
	ticker := s.config.clock.NewTicker(s.config.pingInterval)
	defer ticker.Stop()

	// Send an initial ping immediately so dead-on-arrival connections are
	// detected within writeTimeout instead of waiting a full pingInterval.
	if !s.doPing(ctx, transport) {
		return
	}

	for {
		select {
		case <-ticker.C:
			if !s.doPing(ctx, transport) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// doPing sends a single Ping with a writeTimeout deadline. Returns true if the
// pong arrived successfully, false if the caller should exit.
func (s *session) doPing(ctx context.Context, transport core.Transport) bool {
	pingCtx, cancel := context.WithTimeout(ctx, s.config.writeTimeout)
	err := transport.Ping(pingCtx)
	cancel()
	if err != nil {
		// context.Canceled means the pump context was cancelled (reconnect/close);
		// this is not a pong timeout — exit silently without firing the metric.
		if ctx.Err() != nil {
			return false
		}
		s.config.metrics.PongTimeout(s.roomID, s.id)
		s.config.logger.Debug("wspulse: pingPump stopping: ping failed",
			zap.String("conn_id", s.id), zap.Error(err))
		_ = transport.CloseNow()
		return false
	}
	return true
}

// readPump reads inbound messages from the transport and forwards them to the OnMessage
// callback. When the read loop exits it signals the heart that this transport
// has died. If the heart is shutting down, cleanup is handled inline.
func (s *session) readPump(ctx context.Context, transport core.Transport, h *heart) {
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
		case h.transportDied <- transportDiedMessage{session: s, transport: transport, err: readErr}:
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

	transport.SetReadLimit(s.config.maxMessageSize)

	for {
		_, data, err := transport.Read(ctx)
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
			frame, decodeErr := s.config.codec.Decode(data)
			if decodeErr != nil {
				s.config.logger.Warn("wspulse: decode failed", zap.String("conn_id", s.id), zap.Error(decodeErr))
				continue
			}
			fn(s, frame)
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
// Everything else is abnormal and propagated to OnDisconnect as a
// non-nil error: ping timeout (CloseNow → net error), TCP drops,
// protocol errors, and other read failures.
func isNormalClose(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	code := websocket.CloseStatus(err)
	return code == websocket.StatusNormalClosure ||
		code == websocket.StatusGoingAway
}

// writePump drains the send channel on the transport. writePump is the sole
// goroutine that writes application data to the transport. On exit it
// force-closes the underlying connection so that readPump's Read unblocks.
//
// pumpDone is closed on exit so callers can wait for this pump to finish.
func (s *session) writePump(ctx context.Context, transport core.Transport, pumpDone chan struct{}) {
	defer func() {
		_ = transport.CloseNow()
		close(pumpDone)
	}()

	for {
		// Priority exit: if the context has been cancelled (reconnect swap),
		// stop immediately to avoid consuming messages from s.send that
		// the replacement pump should deliver.
		select {
		case <-ctx.Done():
			return
		default:
		}

		select {
		case data := <-s.send:
			writeCtx, cancel := context.WithTimeout(ctx, s.config.writeTimeout)
			err := transport.Write(writeCtx, s.config.codec.FrameType(), data)
			cancel()
			if err != nil {
				s.config.logger.Warn("wspulse: write failed", zap.String("conn_id", s.id), zap.Error(err))
				return
			}
			s.config.metrics.MessageSent(s.roomID, s.id, len(data))
			s.config.metrics.SendBufferUtilization(s.roomID, s.id, len(s.send), cap(s.send))

		case <-ctx.Done():
			s.config.logger.Debug("wspulse: writePump stopping: context cancelled",
				zap.String("conn_id", s.id))
			// Send a graceful close frame only on session shutdown (s.done
			// closed), not on reconnect swap where speed matters and the
			// old transport may already be dead.
			select {
			case <-s.done:
				_ = transport.Close(core.StatusNormalClosure, "")
			default:
			}
			return
		}
	}
}
