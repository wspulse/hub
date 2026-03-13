package wspulse

import core "github.com/wspulse/core"

// Frame is the minimal transport unit for WebSocket communication.
type Frame = core.Frame

// Codec encodes and decodes Frames for transmission.
type Codec = core.Codec

// JSONCodec is the default Codec. Frames are encoded as JSON text frames.
var JSONCodec = core.JSONCodec

// WebSocket message type constants.
const (
	TextMessage   = core.TextMessage
	BinaryMessage = core.BinaryMessage
)

// Re-exported sentinel errors from github.com/wspulse/core.
var (
	ErrConnectionClosed = core.ErrConnectionClosed
	ErrSendBufferFull   = core.ErrSendBufferFull
)
