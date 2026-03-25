package wspulse

import (
	"errors"
	"fmt"
)

// Server-only sentinel errors. Shared errors (ErrConnectionClosed,
// ErrSendBufferFull) live in github.com/wspulse/core and are re-exported
// in types.go for convenience.
var (
	// ErrConnectionNotFound is returned by Server.Send and Server.Kick
	// when connectionID has no active connection.
	ErrConnectionNotFound = errors.New("wspulse: connection not found")

	// ErrDuplicateConnectionID is passed to the OnDisconnect callback when
	// an existing connection is kicked because a new connection registered
	// with the same connectionID.
	ErrDuplicateConnectionID = errors.New("wspulse: kicked: duplicate connection ID")

	// ErrServerClosed is returned by Server.Broadcast (and potentially
	// other methods) when the Server has already been shut down via Close().
	ErrServerClosed = errors.New("wspulse: server is closed")
)

// PanicError wraps a panic recovered from an OnMessage callback.
// When an OnMessage handler panics, the readPump recovers the panic and
// terminates the connection (or suspends it when session resumption is
// enabled). The transport error (including PanicError) is reported to the
// OnTransportDrop callback so applications can distinguish transport
// failures from handler panics using errors.As.
//
// Behaviour: a panic in OnMessage always kills the connection. This is
// intentional — corrupted handler state should not process further messages.
// When resumption is enabled, OnDisconnect may later be invoked on grace
// expiry with a nil error; callers must not rely on PanicError always being
// delivered via OnDisconnect.
type PanicError struct {
	// Value is the value passed to panic().
	Value any
	// Stack is the goroutine stack trace captured at the point of recovery.
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("wspulse: onMessage panic: %v", e.Value)
}
