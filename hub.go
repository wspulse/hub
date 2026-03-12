package server

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// ── internal message types ────────────────────────────────────────────────────

// registerMessage is sent by ServeHTTP after upgrading a WebSocket connection.
// The hub either creates a new session or attaches the transport to an existing
// suspended session (resume).
type registerMessage struct {
	connectionID string
	roomID       string
	transport    *websocket.Conn
}

// transportDiedMessage is sent by readPump when its WebSocket read loop exits.
// The hub decides whether to suspend the session (resume enabled) or
// destroy it (resume disabled / grace expired).
type transportDiedMessage struct {
	session   *session
	transport *websocket.Conn // the specific transport that died
	err       error           // nil for normal closure
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

// kickRequest is sent by server.Kick to terminate a session.
// Routed through the hub so that map removal, session close, and
// onDisconnect are serialized with all other state mutations.
type kickRequest struct {
	connectionID string
	result       chan error
}

// ── hub ───────────────────────────────────────────────────────────────────────

// hub is the single-goroutine event loop that owns all room and session state.
// External callers interact through channels or mu-guarded read helpers.
//
// All mutations are serialized inside run()'s select loop.
// h.mu (RWMutex) lets external goroutines read rooms/connectionsByID lock-free.
type hub struct {
	rooms           map[string]map[string]*session // roomID → connectionID → session
	connectionsByID map[string]*session            // connectionID → session (flat index)

	register      chan registerMessage
	transportDied chan transportDiedMessage
	graceExpired  chan graceExpiredMessage
	broadcast     chan broadcastMessage
	kick          chan kickRequest
	done          chan struct{} // closed by Server.Close()

	mu      sync.RWMutex
	stopped atomic.Bool // set by shutdown(); ServeHTTP checks this early
	config  *serverConfig
}

func newHub(config *serverConfig) *hub {
	return &hub{
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

// run is the hub's main event loop. It serializes all state mutations.
// Exits when done is closed (via Server.Close()).
func (h *hub) run() {
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
func (h *hub) handleRegister(message registerMessage) {
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

			existing.attachWS(message.transport, h)
			h.config.logger.Info("wspulse: session resumed",
				zap.String("conn_id", message.connectionID),
			)
			return

		case stateConnected:
			// Duplicate connectionID while active — kick the old session.
			h.config.logger.Warn("wspulse: duplicate conn_id, kicking existing session",
				zap.String("conn_id", message.connectionID),
			)
			h.disconnectSession(existing, ErrDuplicateConnectionID)

		case stateClosed:
			// Close() was called externally before the hub processed the
			// resulting cleanup message (graceExpired or transportDied).
			// Fire onDisconnect now; those paths will see the session is
			// no longer registered and skip.
			h.config.logger.Debug("wspulse: stale closed session removed",
				zap.String("conn_id", message.connectionID),
			)
			h.disconnectSession(existing, nil)
		}
	}

	// Create a new session.
	newSession := &session{
		id:     message.connectionID,
		roomID: message.roomID,
		send:   make(chan []byte, h.config.sendBufferSize),
		done:   make(chan struct{}),
		state:  stateConnected,
		config: h.config,
	}
	if h.config.resumeWindow > 0 {
		newSession.resumeBuffer = newRingBuffer(h.config.sendBufferSize)
	}

	h.mu.Lock()
	if h.rooms[message.roomID] == nil {
		h.rooms[message.roomID] = make(map[string]*session)
	}
	h.rooms[message.roomID][message.connectionID] = newSession
	h.connectionsByID[message.connectionID] = newSession
	h.mu.Unlock()

	newSession.attachWS(message.transport, h)

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
func (h *hub) handleTransportDied(message transportDiedMessage) {
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
				h.disconnectSession(target, message.err)
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
			// State is already stateClosed; nothing more to do.
			h.config.logger.Debug("wspulse: detachWS returned not-ok (session closed concurrently)",
				zap.String("conn_id", target.id),
			)
			return
		}
		timer := time.AfterFunc(h.config.resumeWindow, func() {
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
			h.disconnectSession(target, nil)
			return
		}
		target.graceTimer = timer
		target.mu.Unlock()

		h.config.logger.Info("wspulse: session suspended",
			zap.String("conn_id", target.id),
			zap.Duration("resume_window", h.config.resumeWindow),
		)
		return
	}

	// Resume disabled — destroy immediately.
	h.config.logger.Debug("wspulse: session destroyed (resume disabled)",
		zap.String("conn_id", target.id),
		zap.Error(message.err),
	)
	h.disconnectSession(target, message.err)
}

// handleGraceExpired destroys a session whose resume window has elapsed
// without reconnection. Also handles the case where the application called
// Connection.Close() while the session was suspended — the session is in
// stateClosed but still registered in the hub maps.
func (h *hub) handleGraceExpired(message graceExpiredMessage) {
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
	} else {
		h.config.logger.Info("wspulse: suspended session closed by application",
			zap.String("conn_id", target.id),
		)
	}
	h.disconnectSession(target, nil)
}

// handleKick removes a session, closes it, and fires onDisconnect.
// Serialized inside the hub so suspended sessions are cleaned up correctly.
func (h *hub) handleKick(request kickRequest) {
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

	h.disconnectSession(target, nil)
	request.result <- nil
}

// handleBroadcast fans out pre-encoded data to every session in the room.
func (h *hub) handleBroadcast(message broadcastMessage) {
	h.mu.RLock()
	room := h.rooms[message.roomID]
	sessions := make([]*session, 0, len(room))
	for _, s := range room {
		sessions = append(sessions, s)
	}
	h.mu.RUnlock()

	if len(sessions) == 0 {
		h.config.logger.Debug("wspulse: broadcast to empty/unknown room",
			zap.String("room_id", message.roomID),
		)
		return
	}

	for _, target := range sessions {
		select {
		case <-target.done:
			continue
		default:
		}

		// Use enqueue with drop-oldest so suspended sessions buffer to
		// resumeBuffer and connected sessions apply backpressure uniformly.
		_ = target.enqueue(message.data, true)
	}
	h.config.logger.Debug("wspulse: broadcast dispatched",
		zap.String("room_id", message.roomID),
		zap.Int("recipients", len(sessions)),
	)
}

// disconnectSession removes the session from hub maps, closes it, and fires
// onDisconnect. Safe to call even if Close() was already called externally
// (closeOnce makes it idempotent).
func (h *hub) disconnectSession(target *session, err error) {
	h.removeSession(target)
	_ = target.Close()
	if fn := h.config.onDisconnect; fn != nil {
		go fn(target, err)
	}
}

// removeSession removes session from the hub maps and cancels any grace timer.
func (h *hub) removeSession(target *session) {
	target.cancelGraceTimer()

	h.mu.Lock()
	if room := h.rooms[target.roomID]; room != nil && room[target.id] == target {
		delete(room, target.id)
		if len(room) == 0 {
			delete(h.rooms, target.roomID)
		}
	}
	if h.connectionsByID[target.id] == target {
		delete(h.connectionsByID, target.id)
	}
	h.mu.Unlock()
}

// shutdown closes every active session. Called once by run() when done fires.
func (h *hub) shutdown() {
	// Mark stopped before draining so that concurrent ServeHTTP calls
	// bail out early instead of pushing into the register channel.
	h.stopped.Store(true)

	var disconnected []*session

	h.mu.Lock()
	sessionCount := 0
	for _, room := range h.rooms {
		sessionCount += len(room)
	}
	h.config.logger.Info("wspulse: hub shutting down",
		zap.Int("active_sessions", sessionCount),
	)
	for _, room := range h.rooms {
		for _, target := range room {
			target.cancelGraceTimer()
			_ = target.Close()
			disconnected = append(disconnected, target)
		}
	}
	h.rooms = make(map[string]map[string]*session)
	h.connectionsByID = make(map[string]*session)
	h.mu.Unlock()

	// Drain in-flight register messages to prevent goroutine leaks.
	for {
		select {
		case message := <-h.register:
			_ = message.transport.Close()
		default:
			goto drained
		}
	}
drained:

	if h.config.onDisconnect != nil {
		for _, target := range disconnected {
			fn := h.config.onDisconnect
			s := target
			go fn(s, ErrServerClosed)
		}
	}
	h.config.logger.Info("wspulse: hub shutdown complete",
		zap.Int("disconnected", len(disconnected)),
	)
}

// get returns the session for connectionID, or nil. O(1) via flat index.
func (h *hub) get(connectionID string) *session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.connectionsByID[connectionID]
}

// getConnections returns a snapshot of all registered Connection instances in roomID.
func (h *hub) getConnections(roomID string) []Connection {
	h.mu.RLock()
	defer h.mu.RUnlock()
	room := h.rooms[roomID]
	out := make([]Connection, 0, len(room))
	for _, s := range room {
		out = append(out, s)
	}
	return out
}
