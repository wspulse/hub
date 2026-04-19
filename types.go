package wspulse

import core "github.com/wspulse/core"

// Message is the minimal transport unit for WebSocket communication.
type Message = core.Message

// Codec encodes and decodes Messages for transmission.
type Codec = core.Codec

// JSONCodec is the default Codec. Messages are encoded as JSON text frames.
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
