package wspulse_test

import (
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	wspulse "github.com/wspulse/server"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// injectAndWait creates a mock transport, injects it into the server, and
// waits for the onConnect callback to fire. Returns the mock transport.
func injectAndWait(t *testing.T, srv wspulse.Hub, connectionID, roomID string, connected chan struct{}) *mockTransport {
	t.Helper()
	mt := newMockTransport()
	wspulse.InjectTransport(srv, connectionID, roomID, mt)
	<-connected
	return mt
}

// ── OnConnect ────────────────────────────────────────────────────────────────

func TestOnConnect_SendsFrame(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	srv := wspulse.NewHub(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			_ = connection.Send(wspulse.Frame{Event: "welcome", Payload: []byte(`"hello"`)})
			connected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)

	// writePump encodes the frame and writes to mock transport.
	w, ok := mt.WaitWrite(time.Second)
	require.True(t, ok, "expected a write from writePump")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "welcome", f.Event)
}

// ── OnMessage ────────────────────────────────────────────────────────────────

func TestOnMessage_CallbackFires(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	received := make(chan wspulse.Frame, 1)
	srv := wspulse.NewHub(
		acceptAll,
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnMessage(func(_ wspulse.Connection, f wspulse.Frame) {
			received <- f
		}),
	)
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)

	// Inject a message into readPump via mock transport.
	encoded, err := wspulse.JSONCodec.Encode(wspulse.Frame{Event: "msg", Payload: []byte(`{"text":"hello"}`)})
	require.NoError(t, err)
	mt.InjectMessage(websocket.TextMessage, encoded)

	f := requireReceive(t, received)
	assert.Equal(t, "msg", f.Event)
}

// ── Broadcast ────────────────────────────────────────────────────────────────

func TestBroadcast_ReachesConnectedClient(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	srv := wspulse.NewHub(
		acceptAll,
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)

	frame := wspulse.Frame{Event: "notice", Payload: []byte(`"hello room"`)}
	require.NoError(t, srv.Broadcast("test-room", frame))

	w, ok := mt.WaitWrite(time.Second)
	require.True(t, ok, "expected broadcast write")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "notice", f.Event)
}

// ── Send ─────────────────────────────────────────────────────────────────────

func TestSend_DeliversFrameToConnection(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	srv := wspulse.NewHub(
		acceptAll,
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)

	frame := wspulse.Frame{Event: "direct", Payload: []byte(`"hi"`)}
	require.NoError(t, srv.Send("test-connection", frame))

	w, ok := mt.WaitWrite(time.Second)
	require.True(t, ok, "expected direct send write")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "direct", f.Event)
}

// ── OnDisconnect ─────────────────────────────────────────────────────────────

func TestOnDisconnect_CallbackFires(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	srv := wspulse.NewHub(
		acceptAll,
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			disconnected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)

	// Simulate transport drop — readPump exits, triggers disconnect.
	mt.InjectError(errors.New("connection closed"))

	requireReceive(t, disconnected)
}

// ── Kick ─────────────────────────────────────────────────────────────────────

func TestKick_ClosesConnection(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	srv := wspulse.NewHub(
		acceptAll,
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			disconnected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	_ = injectAndWait(t, srv, "test-connection", "test-room", connected)

	require.NoError(t, srv.Kick("test-connection"))

	requireReceive(t, disconnected)
}

// ── GetConnections ───────────────────────────────────────────────────────────

func TestGetConnections_ReturnsRegisteredConnection(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	srv := wspulse.NewHub(
		acceptAll,
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	_ = injectAndWait(t, srv, "test-connection", "test-room", connected)

	connections := srv.GetConnections("test-room")
	require.Len(t, connections, 1)
	assert.Equal(t, "test-connection", connections[0].ID())
	assert.Equal(t, "test-room", connections[0].RoomID())
}

// ── Duplicate Connection ID ──────────────────────────────────────────────────

func TestDuplicateConnectionID_OldKickedNewReachable(t *testing.T) {
	t.Parallel()
	var connectCount atomic.Int32
	connected := make(chan struct{}, 2)
	kicked := make(chan struct{}, 1)
	srv := wspulse.NewHub(
		acceptAll,
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connectCount.Add(1)
			connected <- struct{}{}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, err error) {
			if errors.Is(err, wspulse.ErrDuplicateConnectionID) {
				kicked <- struct{}{}
			}
		}),
	)
	t.Cleanup(srv.Close)

	// First connection.
	_ = injectAndWait(t, srv, "test-connection", "test-room", connected)

	// Second connection with same ID — first should be kicked.
	mt2 := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt2)

	requireReceive(t, kicked)
	requireReceive(t, connected)

	// Verify second connection is reachable.
	frame := wspulse.Frame{Event: "ok", Payload: []byte(`"after-kick"`)}
	require.NoError(t, srv.Send("test-connection", frame))
	w, ok := mt2.WaitWrite(time.Second)
	require.True(t, ok, "expected write to second connection")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "ok", f.Event)
}

// ── Connection.Done ──────────────────────────────────────────────────────────

func TestConnectionDone_ClosedOnKick(t *testing.T) {
	t.Parallel()
	connected := make(chan wspulse.Connection, 1)
	srv := wspulse.NewHub(
		acceptAll,
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			connected <- c
		}),
	)
	t.Cleanup(srv.Close)

	mt := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt)

	conn := requireReceive(t, connected)

	require.NoError(t, srv.Kick(conn.ID()))

	requireReceive(t, conn.Done())
}

// ── Broadcast to empty room ─────────────────────────────────────────────────

func TestBroadcast_EmptyRoom_NoError(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll)
	t.Cleanup(srv.Close)
	err := srv.Broadcast("nonexistent-room", wspulse.Frame{Event: "msg"})
	require.NoError(t, err)
}

// ── Multiple rooms ───────────────────────────────────────────────────────────

func TestMultipleRooms_BroadcastIsolation(t *testing.T) {
	t.Parallel()
	connectedA := make(chan struct{}, 1)
	connectedB := make(chan struct{}, 1)

	var connectCount atomic.Int32
	srv := wspulse.NewHub(
		func(r *http.Request) (string, string, error) {
			n := connectCount.Add(1)
			if n == 1 {
				return "room-a", "conn-a", nil
			}
			return "room-b", "conn-b", nil
		},
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			if c.RoomID() == "room-a" {
				connectedA <- struct{}{}
			} else {
				connectedB <- struct{}{}
			}
		}),
	)
	t.Cleanup(srv.Close)

	mtA := injectAndWait(t, srv, "conn-a", "room-a", connectedA)
	mtB := injectAndWait(t, srv, "conn-b", "room-b", connectedB)

	// Broadcast to room-a only.
	require.NoError(t, srv.Broadcast("room-a", wspulse.Frame{Event: "hello"}))

	// room-a client should receive it.
	w, ok := mtA.WaitWrite(time.Second)
	require.True(t, ok, "room-a should receive broadcast")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "hello", f.Event)

	// room-b client should NOT receive any data frames.
	// Use WaitWrite with short timeout instead of time.Sleep.
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		wb, ok := mtB.WaitWrite(remaining)
		if !ok {
			break // no more writes within window
		}
		if wb.messageType == websocket.TextMessage {
			assert.Fail(t, "room-b should not receive room-a broadcast")
			break
		}
	}
}

// ── Shutdown fires OnDisconnect ─────────────────────────────────────────────

func TestShutdownFiresOnDisconnect(t *testing.T) {
	t.Parallel()
	const count = 3
	connected := make(chan struct{}, count)
	disconnected := make(chan error, count)

	connIndex := 0
	srv := wspulse.NewHub(
		func(r *http.Request) (string, string, error) {
			connIndex++
			return "room", fmt.Sprintf("conn-%d", connIndex), nil
		},
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, err error) {
			disconnected <- err
		}),
	)

	for i := 0; i < count; i++ {
		mt := newMockTransport()
		wspulse.InjectTransport(srv, fmt.Sprintf("conn-%d", i+1), "room", mt)
		requireReceive(t, connected)
	}

	srv.Close()

	for i := 0; i < count; i++ {
		err := requireReceive(t, disconnected)
		assert.ErrorIs(t, err, wspulse.ErrHubClosed)
	}
}

// ── Backpressure: send buffer full ──────────────────────────────────────────

func TestConnectionSend_BufferFull_ReturnsErrSendBufferFull(t *testing.T) {
	t.Parallel()
	connected := make(chan wspulse.Connection, 1)
	srv := wspulse.NewHub(
		acceptAll,
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			connected <- c
		}),
		wspulse.WithSendBufferSize(1),
	)
	t.Cleanup(srv.Close)

	mt := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt)

	conn := requireReceive(t, connected)

	// Rapid-fire send to fill the buffer.
	var gotBufferFull bool
	for i := 0; i < 200; i++ {
		err := conn.Send(wspulse.Frame{Event: "flood"})
		if errors.Is(err, wspulse.ErrSendBufferFull) {
			gotBufferFull = true
			break
		}
	}
	require.True(t, gotBufferFull, "expected ErrSendBufferFull")
}
