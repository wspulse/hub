package wspulse

import (
	"context"

	"github.com/coder/websocket"

	core "github.com/wspulse/core"
)

// transport abstracts the WebSocket connection for testability.
// The API is context-based: deadlines are expressed via context cancellation
// rather than explicit SetReadDeadline / SetWriteDeadline calls.
//
// Implementations must be comparable (== / !=). The heart uses interface
// equality to detect stale transport-died notifications (see
// handleTransportDied in heart.go). Pointer receiver types satisfy this
// requirement naturally.
type transport interface {
	// Read reads the next message from the connection.
	// Blocks until a message arrives, ctx is cancelled, or the connection closes.
	Read(ctx context.Context) (core.MessageType, []byte, error)

	// Write sends a message to the connection.
	// ctx may carry a deadline for the write operation.
	Write(ctx context.Context, typ core.MessageType, data []byte) error

	// Ping sends a ping to the peer and waits for a pong.
	// ctx may carry a deadline; if the pong does not arrive before the deadline,
	// Ping returns an error and the connection should be considered dead.
	Ping(ctx context.Context) error

	// SetReadLimit sets the maximum size in bytes for a single message read
	// from the connection. Messages exceeding this limit are rejected.
	SetReadLimit(n int64)

	// Close performs the WebSocket close handshake with the given status code
	// and reason, then closes the underlying connection.
	Close(code core.StatusCode, reason string) error

	// CloseNow closes the underlying connection immediately without
	// attempting a close handshake. Used in defer paths and error teardown.
	CloseNow() error
}

// coderTransport wraps *websocket.Conn to satisfy the hub-local transport interface.
// All conversions are thin type casts — MessageType and StatusCode values
// are identical to RFC 6455, so no runtime calculation is needed.
type coderTransport struct{ c *websocket.Conn }

func (t *coderTransport) Read(ctx context.Context) (core.MessageType, []byte, error) {
	typ, data, err := t.c.Read(ctx)
	return core.MessageType(typ), data, err
}

func (t *coderTransport) Write(ctx context.Context, typ core.MessageType, data []byte) error {
	return t.c.Write(ctx, websocket.MessageType(typ), data)
}

func (t *coderTransport) Ping(ctx context.Context) error {
	return t.c.Ping(ctx)
}

func (t *coderTransport) SetReadLimit(n int64) {
	t.c.SetReadLimit(n)
}

func (t *coderTransport) Close(code core.StatusCode, reason string) error {
	return t.c.Close(websocket.StatusCode(code), reason)
}

func (t *coderTransport) CloseNow() error {
	return t.c.CloseNow()
}
