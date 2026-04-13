package wspulse

import (
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	core "github.com/wspulse/core"
	"github.com/wspulse/hub/ringbuffer"
)

// ── internal message types ────────────────────────────────────────────────────

// registerMessage is sent by ServeHTTP after upgrading a WebSocket connection.
// The heart either creates a new session or attaches the transport to an existing
// suspended session (resume).
type registerMessage struct {
	connectionID string
	roomID       string
	transport    core.Transport
}

// transportDiedMessage is sent by readPump when its WebSocket read loop exits.
// The heart decides whether to suspend the session (resume enabled) or
// destroy it (resume disabled / grace expired).
type transportDiedMessage struct {
	session   *session
	transport core.Transport // the specific transport that died
	err       error          // nil for normal closure
}

// graceExpiredMessage is sent by a time.AfterFunc when the resume window elapses
// without reconnection. epoch matches session.suspendEpoch at the time the
// timer was created; stale timers from previous suspend/resume cycles are
// detected by comparing epochs.
type graceExpiredMessage struct {
	session *session
	epoch   uint64
}

type broadcastMessage struct {
	roomID string
	data   []byte
}

// kickRequest is sent by Hub.Kick to terminate a session.
// Routed through the heart so that map removal, session close, and
// onDisconnect are serialized with all other state mutations.
type kickRequest struct {
	connectionID string
	result       chan error
}

// ── heart ───────────────────────────────────────────────────────────────────────

// heart is the single-goroutine event loop that owns all room and session state.
// External callers interact through channels or mu-guarded read helpers.
//
// All mutations are serialized inside run()'s select loop.
// h.mu (RWMutex) lets external goroutines read rooms/connectionsByID under RLock.
type heart struct {
	rooms           map[string]map[string]*session // roomID → connectionID → session
	connectionsByID map[string]*session            // connectionID → session (flat index)

	register      chan registerMessage
	transportDied chan transportDiedMessage
	graceExpired  chan graceExpiredMessage
	broadcast     chan broadcastMessage
	kick          chan kickRequest
	done          chan struct{} // closed by Hub.Close()

	mu      sync.RWMutex
	stopped atomic.Bool // set by shutdown(); ServeHTTP checks this early
	scratch []*session  // reusable slice for broadcast snapshot; avoids per-broadcast allocation
	config  *hubConfig
}

func newHeart(config *hubConfig) *heart {
	return &heart{
		rooms:           make(map[string]map[string]*session),
		connectionsByID: make(map[string]*session),
		register:        make(chan registerMessage, 64),
		transportDied:   make(chan transportDiedMessage, 64),
		graceExpired:    make(chan graceExpiredMessage, 64),
		broadcast:       make(chan broadcastMessage, 256),
		kick:            make(chan kickRequest, 16),
		done:            make(chan struct{}),
		config:          config,
	}
}

// run is the heart's main event loop. It serializes all state mutations.
// Exits when done is closed (via Hub.Close()).
func (h *heart) run() {
	for {
		select {
		case message := <-h.register:
			h.handleRegister(message)

		case message := <-h.transportDied:
			h.handleTransportDied(message)

		case message := <-h.graceExpired:
			h.handleGraceExpired(message)

		case message := <-h.broadcast:
			h.handleBroadcast(message)

		case request := <-h.kick:
			h.handleKick(request)

		case <-h.done:
			h.shutdown()
			return
		}
	}
}

// handleRegister processes a new WebSocket connection.
// If an existing suspended session matches the connectionID, the transport is swapped in
// (session resume). Otherwise a new session is created.
func (h *heart) handleRegister(message registerMessage) {
	h.mu.RLock()
	existing, exists := h.connectionsByID[message.connectionID]
	h.mu.RUnlock()

	if exists {
		existing.mu.Lock()
		state := existing.state
		existing.mu.Unlock()

		switch state {
		case stateSuspended:
			// Resume: cancel the grace timer and attach the new transport.
			// Bump suspendEpoch so that a graceExpiredMessage that was
			// already enqueued (timer fired before Stop) is detected
			// as stale and ignored by handleGraceExpired.
			existing.cancelGraceTimer()

			var onResume func()
			if fn := h.config.onTransportRestore; fn != nil {
				onResume = func() { fn(existing) }
			}
			h.config.metrics.ResumeAttempt(existing.roomID, existing.id)
			existing.attachWS(message.transport, h, onResume)
			h.config.logger.Info("wspulse: session resumed",
				zap.String("conn_id", message.connectionID),
			)
			return

		case stateConnected:
			// Duplicate connectionID while active — kick the old session.
			h.config.logger.Warn("wspulse: duplicate conn_id, kicking existing session",
				zap.String("conn_id", message.connectionID),
			)
			h.disconnectSession(existing, ErrDuplicateConnectionID, DisconnectDuplicate)

		case stateClosed:
			// Close() was called externally before the heart processed the
			// resulting cleanup message (graceExpired or transportDied).
			// Fire onDisconnect now; those paths will see the session is
			// no longer registered and skip.
			h.config.logger.Debug("wspulse: stale closed session removed",
				zap.String("conn_id", message.connectionID),
			)
			h.disconnectSession(existing, nil, DisconnectNormal)
		}
	}

	// Create a new session.
	newSession := &session{
		id:          message.connectionID,
		roomID:      message.roomID,
		send:        newSendQueue(h.config.sendBufferSize),
		done:        make(chan struct{}),
		state:       stateConnected,
		connectedAt: time.Now(),
		config:      h.config,
	}
	if h.config.resumeWindow > 0 {
		newSession.resumeBuffer = ringbuffer.New[[]byte](h.config.sendBufferSize)
	}

	var roomCreated bool
	h.mu.Lock()
	if h.rooms[message.roomID] == nil {
		h.rooms[message.roomID] = make(map[string]*session)
		roomCreated = true
	}
	h.rooms[message.roomID][message.connectionID] = newSession
	h.connectionsByID[message.connectionID] = newSession
	h.mu.Unlock()
	if roomCreated {
		h.config.metrics.RoomCreated(message.roomID)
	}

	newSession.attachWS(message.transport, h, nil)
	h.config.metrics.ConnectionOpened(message.roomID, message.connectionID)

	h.config.logger.Debug("wspulse: session connected",
		zap.String("conn_id", message.connectionID),
		zap.String("room_id", message.roomID),
	)

	if fn := h.config.onConnect; fn != nil {
		go fn(newSession)
	}
}

// handleTransportDied processes a dead WebSocket transport.
// If resume is enabled and the session state is stateConnected, transition
// to stateSuspended and start the grace timer. Otherwise destroy the session.
func (h *heart) handleTransportDied(message transportDiedMessage) {
	target := message.session

	// Stale notification: the ws that died is no longer the current ws
	// (already swapped by a reconnect). Ignore.
	target.mu.Lock()
	if target.transport != message.transport {
		target.mu.Unlock()
		h.config.logger.Debug("wspulse: stale transport-died ignored (transport already swapped)",
			zap.String("conn_id", target.id),
		)
		return
	}
	state := target.state
	target.mu.Unlock()

	if state != stateConnected {
		switch state {
		case stateSuspended:
			// The readPump for a new transport (started by attachWS) died while
			// the transition goroutine was still draining the resume
			// buffer (state is still stateSuspended). Nil out the transport so
			// the transition goroutine's transport-validity check prevents it
			// from starting a writePump on the dead connection.
			target.mu.Lock()
			if target.transport == message.transport {
				target.transport = nil
			}
			target.mu.Unlock()
			h.config.logger.Warn("wspulse: transport died while session still suspended (mid-transition)",
				zap.String("conn_id", target.id),
			)

		case stateClosed:
			// Session was already closed (e.g. via Kick) before readPump
			// reported the transport death. Clean up maps and fire
			// onDisconnect only if the session is still registered (not
			// already removed by handleRegister's duplicate-kick path).
			h.mu.RLock()
			stillRegistered := h.connectionsByID[target.id] == target
			h.mu.RUnlock()
			if stillRegistered {
				h.config.logger.Debug("wspulse: transport-died for closed session, cleaning up",
					zap.String("conn_id", target.id),
				)
				h.disconnectSession(target, message.err, DisconnectNormal)
			} else {
				h.config.logger.Debug("wspulse: transport-died for unregistered closed session, skipping",
					zap.String("conn_id", target.id),
				)
			}
		}
		return
	}

	if h.config.resumeWindow > 0 {
		epoch, ok := target.detachWS()
		if !ok {
			// Session was concurrently closed (e.g. via external Close()).
			// Close() only sets stateClosed — it does not remove the session
			// from heart maps or fire onDisconnect. Do that here.
			h.config.logger.Debug("wspulse: detachWS returned not-ok (session closed concurrently)",
				zap.String("conn_id", target.id),
			)
			h.disconnectSession(target, message.err, DisconnectNormal)
			return
		}
		timer := h.config.clock.AfterFunc(h.config.resumeWindow, func() {
			select {
			case h.graceExpired <- graceExpiredMessage{session: target, epoch: epoch}:
			case <-h.done:
			}
		})
		target.mu.Lock()
		// Re-check state: Close() may have set stateClosed between
		// detachWS() and here. In that case graceTimer is still nil,
		// so Close()'s timer.Reset(0) never fired. Handle it inline.
		if target.state == stateClosed {
			target.mu.Unlock()
			timer.Stop()
			h.config.logger.Info("wspulse: suspended session closed by application (race path)",
				zap.String("conn_id", target.id),
			)
			// Pass message.err (not nil): OnTransportDrop was never called
			// because detachWS succeeded but Close() raced in before the
			// timer was assigned. The transport error must reach OnDisconnect
			// so it is delivered exactly once across the two callbacks.
			h.disconnectSession(target, message.err, DisconnectNormal)
			return
		}
		target.graceTimer = timer
		target.mu.Unlock()

		h.config.logger.Info("wspulse: session suspended",
			zap.String("conn_id", target.id),
			zap.Duration("resume_window", h.config.resumeWindow),
		)
		if fn := h.config.onTransportDrop; fn != nil {
			go fn(target, message.err)
		}
		return
	}

	// Resume disabled — destroy immediately.
	h.config.logger.Debug("wspulse: session destroyed (resume disabled)",
		zap.String("conn_id", target.id),
		zap.Error(message.err),
	)
	h.disconnectSession(target, message.err, DisconnectNormal)
}

// handleGraceExpired destroys a session whose resume window has elapsed
// without reconnection. Also handles the case where the application called
// Connection.Close() while the session was suspended — the session is in
// stateClosed but still registered in the heart maps.
func (h *heart) handleGraceExpired(message graceExpiredMessage) {
	target := message.session

	target.mu.Lock()
	state := target.state
	epoch := target.suspendEpoch
	target.mu.Unlock()

	// Stale timer from a previous suspend/resume cycle.
	if message.epoch != epoch {
		h.config.logger.Debug("wspulse: stale grace timer ignored",
			zap.String("conn_id", target.id),
			zap.Uint64("msg_epoch", message.epoch),
			zap.Uint64("current_epoch", epoch),
		)
		return
	}

	// Act on stateSuspended (normal expiry) and stateClosed (application
	// called Close() while suspended). Skip stateConnected — the session
	// was successfully resumed and the timer is outdated.
	if state == stateConnected {
		h.config.logger.Debug("wspulse: grace timer fired but session already resumed",
			zap.String("conn_id", target.id),
		)
		return
	}

	// For stateSuspended: normal window expiry — Close() then onDisconnect.
	// For stateClosed: application called Close() on the suspended session,
	// which Reset the timer to 0 to force immediate expiry.
	if state == stateSuspended {
		h.config.logger.Info("wspulse: session expired",
			zap.String("conn_id", target.id),
		)
		h.disconnectSession(target, nil, DisconnectGraceExpired)
	} else {
		h.config.logger.Info("wspulse: suspended session closed by application",
			zap.String("conn_id", target.id),
		)
		h.disconnectSession(target, nil, DisconnectNormal)
	}
}

// handleKick removes a session, closes it, and fires onDisconnect.
// Serialized inside the heart so suspended sessions are cleaned up correctly.
func (h *heart) handleKick(request kickRequest) {
	h.mu.RLock()
	target, exists := h.connectionsByID[request.connectionID]
	h.mu.RUnlock()

	if !exists {
		h.config.logger.Debug("wspulse: kick for unknown conn_id",
			zap.String("conn_id", request.connectionID),
		)
		request.result <- ErrConnectionNotFound
		return
	}

	h.config.logger.Debug("wspulse: session kicked",
		zap.String("conn_id", request.connectionID),
	)

	h.disconnectSession(target, nil, DisconnectKick)
	request.result <- nil
}

// handleBroadcast fans out pre-encoded data to every session in the room.
// Uses h.scratch to avoid allocating a snapshot slice per broadcast.
// Safe because the heart event loop is single-threaded.
func (h *heart) handleBroadcast(message broadcastMessage) {
	h.mu.RLock()
	room := h.rooms[message.roomID]
	h.scratch = h.scratch[:0]
	for _, s := range room {
		h.scratch = append(h.scratch, s)
	}
	h.mu.RUnlock()

	if len(h.scratch) == 0 {
		h.config.logger.Debug("wspulse: broadcast to empty/unknown room",
			zap.String("room_id", message.roomID),
		)
		return
	}

	enqueued := 0
	for _, target := range h.scratch {
		select {
		case <-target.done:
			continue
		default:
		}

		// Use enqueue with drop-oldest so suspended sessions buffer to
		// resumeBuffer and connected sessions apply backpressure uniformly.
		_ = target.enqueue(message.data, true)
		enqueued++
	}
	h.config.metrics.MessageBroadcast(message.roomID, len(message.data), enqueued)
	h.config.logger.Debug("wspulse: broadcast dispatched",
		zap.String("room_id", message.roomID),
		zap.Int("recipients", enqueued),
	)

	// Clear pointers so disconnected sessions can be GC'd.
	// Without this, the backing array retains stale *session pointers
	// in its capacity area after h.scratch[:0] on the next broadcast.
	for i := range h.scratch {
		h.scratch[i] = nil
	}
}

// disconnectSession removes the session from heart maps, closes it, and fires
// onDisconnect. Safe to call even if Close() was already called externally
// (closeOnce makes it idempotent).
func (h *heart) disconnectSession(target *session, err error, reason DisconnectReason) {
	h.removeSession(target)
	h.config.metrics.ConnectionClosed(target.roomID, target.id, time.Since(target.connectedAt), reason)
	_ = target.Close()
	if fn := h.config.onDisconnect; fn != nil {
		go fn(target, err)
	}
}

// removeSession removes session from the heart maps and cancels any grace timer.
func (h *heart) removeSession(target *session) {
	target.cancelGraceTimer()

	var roomDestroyed bool
	h.mu.Lock()
	if room := h.rooms[target.roomID]; room != nil && room[target.id] == target {
		delete(room, target.id)
		if len(room) == 0 {
			delete(h.rooms, target.roomID)
			roomDestroyed = true
		}
	}
	if h.connectionsByID[target.id] == target {
		delete(h.connectionsByID, target.id)
	}
	h.mu.Unlock()
	if roomDestroyed {
		h.config.metrics.RoomDestroyed(target.roomID)
	}
}

// shutdown closes every active session. Called once by run() when done fires.
func (h *heart) shutdown() {
	// Mark stopped before draining so that concurrent ServeHTTP calls
	// bail out early instead of pushing into the register channel.
	h.stopped.Store(true)

	var disconnected []*session

	h.mu.Lock()
	sessionCount := 0
	for _, room := range h.rooms {
		sessionCount += len(room)
	}
	h.config.logger.Info("wspulse: heart shutting down",
		zap.Int("active_sessions", sessionCount),
	)
	type closedInfo struct {
		roomID       string
		connectionID string
		duration     time.Duration
	}
	var closedInfos []closedInfo
	var destroyedRooms []string
	for roomID, room := range h.rooms {
		for _, target := range room {
			target.cancelGraceTimer()
			closedInfos = append(closedInfos, closedInfo{target.roomID, target.id, time.Since(target.connectedAt)})
			_ = target.Close()
			disconnected = append(disconnected, target)
		}
		destroyedRooms = append(destroyedRooms, roomID)
	}
	h.rooms = make(map[string]map[string]*session)
	h.connectionsByID = make(map[string]*session)
	h.mu.Unlock()

	// Emit metrics outside the lock to avoid deadlocks if the
	// MetricsCollector calls back into server APIs.
	for _, info := range closedInfos {
		h.config.metrics.ConnectionClosed(info.roomID, info.connectionID, info.duration, DisconnectHubClose)
	}
	for _, roomID := range destroyedRooms {
		h.config.metrics.RoomDestroyed(roomID)
	}

	// Drain in-flight register messages to prevent goroutine leaks.
	for {
		select {
		case message := <-h.register:
			_ = message.transport.CloseNow()
		default:
			goto drained
		}
	}
drained:

	if h.config.onDisconnect != nil {
		for _, target := range disconnected {
			fn := h.config.onDisconnect
			s := target
			go fn(s, ErrHubClosed)
		}
	}
	h.config.logger.Info("wspulse: heart shutdown complete",
		zap.Int("disconnected", len(disconnected)),
	)
}

// get returns the session for connectionID, or nil. O(1) via flat index.
func (h *heart) get(connectionID string) *session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.connectionsByID[connectionID]
}

// getConnections returns a snapshot of all registered Connection instances in roomID.
func (h *heart) getConnections(roomID string) []Connection {
	h.mu.RLock()
	defer h.mu.RUnlock()
	room := h.rooms[roomID]
	out := make([]Connection, 0, len(room))
	for _, s := range room {
		out = append(out, s)
	}
	return out
}
