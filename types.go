package wspulse

import wspulse "github.com/wspulse/core"

// Frame is the minimal transport unit for WebSocket communication.
type Frame = wspulse.Frame

// Codec encodes and decodes Frames for transmission.
type Codec = wspulse.Codec

// JSONCodec is the default Codec. Frames are encoded as JSON text frames.
var JSONCodec = wspulse.JSONCodec

// WebSocket message type constants.
const (
	TextMessage   = wspulse.TextMessage
	BinaryMessage = wspulse.BinaryMessage
)

// Re-exported sentinel errors from github.com/wspulse/core.
var (
	ErrConnectionClosed = wspulse.ErrConnectionClosed
	ErrSendBufferFull   = wspulse.ErrSendBufferFull
)
