package wspulse

import "errors"

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
