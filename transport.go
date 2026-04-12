package wspulse

import (
	"context"

	"github.com/coder/websocket"

	core "github.com/wspulse/core"
)

// coderTransport wraps *websocket.Conn to satisfy core.Transport.
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
