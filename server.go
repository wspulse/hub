package wspulse

import (
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// Server is the public interface for the WebSocket server.
// Callers depend on this interface rather than the concrete implementation.
type Server interface {
	http.Handler

	// Send enqueues a Frame for the connection identified by connectionID.
	// Returns ErrConnectionNotFound if connectionID has no active connection.
	Send(connectionID string, f Frame) error

	// Broadcast enqueues a Frame for every active connection in roomID.
	Broadcast(roomID string, f Frame) error

	// Kick forcefully closes the connection identified by connectionID.
	// Kick always bypasses the resume window — the session is destroyed
	// immediately without entering the suspended state.
	// Returns ErrConnectionNotFound if connectionID has no active connection.
	Kick(connectionID string) error

	// GetConnections returns a snapshot of all registered connections in roomID.
	// This includes suspended sessions (within the resume window) that have no
	// active WebSocket transport.
	GetConnections(roomID string) []Connection

	// Close gracefully shuts down the server, terminating all connections.
	Close()
}

// internalServer is the unexported, concrete implementation of Server.
type internalServer struct {
	config    *serverConfig
	hub       *hub
	upgrader  websocket.Upgrader
	closeOnce sync.Once
	hubDone   chan struct{} // closed when the hub goroutine fully exits
}

// verify Server interface is satisfied at compile time.
var _ Server = (*internalServer)(nil)

// NewServer creates and starts a Server. connect must not be nil.
func NewServer(connect ConnectFunc, options ...ServerOption) Server {
	if connect == nil {
		panic("wspulse: NewServer: connect must not be nil")
	}
	config := defaultConfig(connect)
	for _, option := range options {
		option(config)
	}
	h := newHub(config)
	hubDone := make(chan struct{})
	go func() {
		h.run()
		// Final drain: catch register messages that slipped through between
		// shutdown()'s drain and run()'s return. Without this, those
		// WebSocket connections leak (nobody left to consume the channel).
		for {
			select {
			case message := <-h.register:
				config.logger.Warn("wspulse: closing leaked transport from post-shutdown register",
					zap.String("conn_id", message.connectionID),
				)
				_ = message.transport.Close()
			default:
				close(hubDone)
				return
			}
		}
	}()
	srv := &internalServer{
		config:  config,
		hub:     h,
		hubDone: hubDone,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     config.checkOrigin,
		},
	}
	config.logger.Info("wspulse: server started",
		zap.Duration("ping_period", config.pingPeriod),
		zap.Duration("resume_window", config.resumeWindow),
		zap.Int("send_buffer_size", config.sendBufferSize),
	)
	return srv
}

// ServeHTTP upgrades the HTTP connection to WebSocket.
// ConnectFunc is called to authenticate; a non-nil error yields HTTP 401.
func (s *internalServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Quick bail — hub is shutting down; don't upgrade or enqueue.
	if s.hub.stopped.Load() {
		s.config.logger.Warn("wspulse: ServeHTTP rejected — server closed")
		http.Error(w, "server closed", http.StatusServiceUnavailable)
		return
	}

	roomID, connectionID, err := s.config.connect(r)
	if err != nil {
		s.config.logger.Debug("wspulse: connect rejected",
			zap.Error(err),
		)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if connectionID == "" {
		connectionID = uuid.NewString()
	}

	transport, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.config.logger.Error("wspulse: upgrade failed", zap.Error(err))
		return
	}

	message := registerMessage{
		connectionID: connectionID,
		roomID:       roomID,
		transport:    transport,
	}

	s.config.logger.Debug("wspulse: connection upgraded",
		zap.String("conn_id", connectionID),
		zap.String("room_id", roomID),
	)

	// Atomically send registerMessage or bail if hub has stopped.
	select {
	case s.hub.register <- message:
	case <-s.hub.done:
		s.config.logger.Warn("wspulse: hub stopped after upgrade, closing transport",
			zap.String("conn_id", connectionID),
		)
		_ = transport.Close()
		return
	}
}

// Send enqueues a Frame for the connection identified by connectionID.
func (s *internalServer) Send(connectionID string, f Frame) error {
	target := s.hub.get(connectionID)
	if target == nil {
		return ErrConnectionNotFound
	}
	return target.Send(f)
}

// Broadcast enqueues a Frame for every active connection in roomID.
func (s *internalServer) Broadcast(roomID string, f Frame) error {
	select {
	case <-s.hub.done:
		return ErrServerClosed
	default:
	}

	data, err := s.config.codec.Encode(f)
	if err != nil {
		s.config.logger.Warn("wspulse: broadcast encode failed",
			zap.String("room_id", roomID),
			zap.Error(err),
		)
		return err
	}
	select {
	case s.hub.broadcast <- broadcastMessage{roomID: roomID, data: data}:
		return nil
	case <-s.hub.done:
		return ErrServerClosed
	}
}

// Kick forcefully closes the connection identified by connectionID.
// Always bypasses the resume window — the session is destroyed
// immediately without entering the suspended state.
// Routed through the hub so cleanup is serialized with other state mutations.
func (s *internalServer) Kick(connectionID string) error {
	result := make(chan error, 1)
	select {
	case s.hub.kick <- kickRequest{connectionID: connectionID, result: result}:
	case <-s.hub.done:
		return ErrServerClosed
	}
	select {
	case err := <-result:
		return err
	case <-s.hub.done:
		return ErrServerClosed
	}
}

// GetConnections returns a snapshot of all registered connections in roomID.
func (s *internalServer) GetConnections(roomID string) []Connection {
	return s.hub.getConnections(roomID)
}

// Close gracefully shuts down the Server and blocks until the hub
// goroutine has fully exited and all managed resources are released.
// Safe to call multiple times and from multiple goroutines; only the
// first call triggers shutdown, but all calls block until completion.
func (s *internalServer) Close() {
	s.closeOnce.Do(func() {
		s.config.logger.Info("wspulse: server closing")
		close(s.hub.done)
	})
	<-s.hubDone
}
