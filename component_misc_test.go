package wspulse_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	wspulse "github.com/wspulse/server"
)

// ── ReadPump panic recovery ─────────────────────────────────────────────────

func TestComponent_ReadPumpPanicRecovery(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnMessage(func(_ wspulse.Connection, _ wspulse.Frame) {
			panic("boom")
		}),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			disconnected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)

	// Inject a message that triggers the panic in OnMessage.
	encoded, _ := wspulse.JSONCodec.Encode(wspulse.Frame{Event: "trigger"})
	mt.InjectMessage(websocket.TextMessage, encoded)

	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnDisconnect after panic")
	}
}

func TestComponent_ReadPumpPanic_ErrorsAsPanicError(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnectErr := make(chan error, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnMessage(func(_ wspulse.Connection, _ wspulse.Frame) {
			panic("typed-boom")
		}),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, err error) {
			disconnectErr <- err
		}),
	)
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)

	encoded, _ := wspulse.JSONCodec.Encode(wspulse.Frame{Event: "trigger"})
	mt.InjectMessage(websocket.TextMessage, encoded)

	select {
	case got := <-disconnectErr:
		var pe *wspulse.PanicError
		require.ErrorAs(t, got, &pe)
		assert.Equal(t, "typed-boom", pe.Value)
		require.NotEmpty(t, pe.Stack)
		assert.Equal(t, "wspulse: onMessage panic: typed-boom", pe.Error())
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnDisconnect")
	}
}

// ── ReadPump malformed frame ────────────────────────────────────────────────

func TestComponent_ReadPump_MalformedFrame_DropsAndContinues(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	received := make(chan wspulse.Frame, 2)
	srv := wspulse.NewServer(
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

	// Inject malformed frame (not valid JSON).
	mt.InjectMessage(websocket.TextMessage, []byte("not-json"))

	// Inject valid frame after the malformed one.
	encoded, _ := wspulse.JSONCodec.Encode(wspulse.Frame{Event: "valid"})
	mt.InjectMessage(websocket.TextMessage, encoded)

	select {
	case f := <-received:
		assert.Equal(t, "valid", f.Event)
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for valid frame after malformed one")
	}
}

// ── Broadcast drop-oldest with slow client ──────────────────────────────────

func TestComponent_BroadcastDropsOldest_SlowClient(t *testing.T) {
	t.Parallel()
	const bufferSize = 2
	const totalBroadcasts = 200

	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithSendBufferSize(bufferSize),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)

	for i := 0; i < totalBroadcasts; i++ {
		typ := "old"
		if i == totalBroadcasts-1 {
			typ = "newest"
		}
		_ = srv.Broadcast("test-room", wspulse.Frame{Event: typ, Payload: []byte(`"x"`)})
	}
	_ = srv.Broadcast("test-room", wspulse.Frame{Event: "done"})

	// Read from mock transport until sentinel.
	var frames []wspulse.Frame
	for {
		w, ok := mt.WaitWrite(time.Second)
		if !ok {
			require.Fail(t, "timed out waiting for frames")
		}
		// Skip ping frames.
		if w.messageType != websocket.TextMessage {
			continue
		}
		f, err := wspulse.JSONCodec.Decode(w.data)
		if err != nil {
			continue
		}
		if f.Event == "done" {
			break
		}
		frames = append(frames, f)
	}

	require.NotEmpty(t, frames)
	found := false
	for _, f := range frames {
		if f.Event == "newest" {
			found = true
			break
		}
	}
	assert.True(t, found, "newest frame not found; drop-oldest did not preserve it")
}

// ── Concurrent broadcast ────────────────────────────────────────────────────

func TestComponent_ConcurrentBroadcast_NoRace(t *testing.T) {
	t.Parallel()
	const workers = 8
	const messagesPerWorker = 50

	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "room", "", nil
		},
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	_ = injectAndWait(t, srv, "conn-1", "room", connected)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < messagesPerWorker; j++ {
				_ = srv.Broadcast("room", wspulse.Frame{Event: "ping"})
			}
		}()
	}
	wg.Wait()
}

// ── Concurrent close and kick ───────────────────────────────────────────────

func TestComponent_ConcurrentCloseAndKick_NoRace(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)

	_ = injectAndWait(t, srv, "test-connection", "test-room", connected)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		srv.Close()
	}()
	go func() {
		defer wg.Done()
		_ = srv.Kick("test-connection")
	}()
	wg.Wait()
}

// ── Concurrent close and broadcast ──────────────────────────────────────────

func TestComponent_ConcurrentCloseAndBroadcast_NoRace(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)

	_ = injectAndWait(t, srv, "test-connection", "test-room", connected)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = srv.Broadcast("test-room", wspulse.Frame{Event: "msg"})
		}
	}()
	go func() {
		defer wg.Done()
		srv.Close()
	}()
	wg.Wait()
}

// ── Close blocks until hub exits ────────────────────────────────────────────

func TestComponent_Close_BlocksUntilHubExits(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
	)

	_ = injectAndWait(t, srv, "test-connection", "test-room", connected)

	done := make(chan struct{})
	go func() {
		srv.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		require.Fail(t, "Close() did not return within timeout")
	}
}

// ── Connection.Send after Close ─────────────────────────────────────────────

func TestComponent_ConnectionSend_AfterClose_ReturnsErrConnectionClosed(t *testing.T) {
	t.Parallel()
	connected := make(chan wspulse.Connection, 1)
	disconnected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			connected <- c
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			disconnected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	mt := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt)
	var conn wspulse.Connection
	select {
	case conn = <-connected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for connect")
	}

	// Kill transport to trigger disconnect.
	mt.InjectError(errors.New("closed"))
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for disconnect")
	}

	err := conn.Send(wspulse.Frame{Event: "after-close"})
	assert.ErrorIs(t, err, wspulse.ErrConnectionClosed)
}

// ── Broadcast skips closed session ──────────────────────────────────────────

func TestComponent_Broadcast_SkipsClosedSession(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
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
	mt.InjectError(errors.New("closed"))
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for disconnect")
	}

	// Broadcast to the room — should not panic or error despite closed session.
	err := srv.Broadcast("test-room", wspulse.Frame{Event: "after-close"})
	require.NoError(t, err)
}

// ── GetConnections empty after disconnect ───────────────────────────────────

func TestComponent_GetConnections_EmptyAfterDisconnect(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
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
	mt.InjectError(errors.New("closed"))
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for disconnect")
	}

	// Brief sleep for hub to process removal.
	time.Sleep(50 * time.Millisecond)
	connections := srv.GetConnections("test-room")
	assert.Empty(t, connections)
}

// ── Encode error paths ──────────────────────────────────────────────────────

// failingCodec always returns an error on Encode.
type failingCodec struct{}

func (failingCodec) Encode(wspulse.Frame) ([]byte, error) {
	return nil, errors.New("codec: encode failed")
}

func (failingCodec) Decode(data []byte) (wspulse.Frame, error) {
	return wspulse.JSONCodec.Decode(data)
}

func (failingCodec) FrameType() int { return wspulse.TextMessage }

func TestComponent_Broadcast_EncodeError_ReturnsError(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithCodec(failingCodec{}),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	_ = injectAndWait(t, srv, "test-connection", "test-room", connected)

	err := srv.Broadcast("test-room", wspulse.Frame{Event: "test"})
	assert.Error(t, err)
}

func TestComponent_ConnectionSend_EncodeError_ReturnsError(t *testing.T) {
	t.Parallel()
	connected := make(chan wspulse.Connection, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithCodec(failingCodec{}),
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			connected <- c
		}),
	)
	t.Cleanup(srv.Close)

	mt := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt)
	var conn wspulse.Connection
	select {
	case conn = <-connected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for connect")
	}

	err := conn.Send(wspulse.Frame{Event: "test"})
	assert.Error(t, err)
}

// ── No OnMessage — readPump still processes ─────────────────────────────────

func TestComponent_NoOnMessage_ReadPumpStillProcesses(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		// No WithOnMessage.
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			disconnected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)

	// Send a message — readPump should process it without error even with no OnMessage.
	encoded, _ := wspulse.JSONCodec.Encode(wspulse.Frame{Event: "ignored"})
	mt.InjectMessage(websocket.TextMessage, encoded)

	// Then kill the transport.
	mt.InjectError(errors.New("closed"))

	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for disconnect")
	}
}

// ── HTTP-layer tests (use httptest, no mock transport) ──────────────────────

func TestComponent_ConnectFunc_RejectReturns401(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewServer(func(r *http.Request) (string, string, error) {
		return "", "", errors.New("internal: db connection pool exhausted")
	})
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "unauthorized", strings.TrimSpace(string(body)),
		"internal error must not leak")
}

func TestComponent_ServeHTTP_AfterClose_Returns503(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewServer(acceptAll)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	srv.Close()

	resp, err := http.Get(ts.URL)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestComponent_ServeHTTP_EmptyConnectionID_GetsUUID(t *testing.T) {
	t.Parallel()
	connected := make(chan wspulse.Connection, 1)
	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "room", "", nil // empty connectionID → server generates UUID
		},
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			connected <- c
		}),
	)
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	select {
	case conn := <-connected:
		assert.NotEmpty(t, conn.ID(), "expected server-generated UUID")
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for connect")
	}
}

// ── Connection.Send with done closed ────────────────────────────────────────

func TestComponent_ConnectionSend_DoneClosesDuringEnqueue(t *testing.T) {
	t.Parallel()
	connected := make(chan wspulse.Connection, 1)
	disconnected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			connected <- c
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			disconnected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	mt := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt)
	var conn wspulse.Connection
	select {
	case conn = <-connected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for connect")
	}

	// Kick to close the session.
	require.NoError(t, srv.Kick("test-connection"))
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for disconnect")
	}

	// Send after done is closed.
	err := conn.Send(wspulse.Frame{Event: "late"})
	assert.ErrorIs(t, err, wspulse.ErrConnectionClosed)
}

// ── Close while connecting ──────────────────────────────────────────────────

func TestComponent_CloseWhileConnecting_NoLeak(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "room", "", nil
		},
	)

	// Inject multiple transports concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			mt := newMockTransport()
			wspulse.InjectTransport(srv, fmt.Sprintf("conn-%d", n), "room", mt)
			// Keep readPump alive until server closes.
			<-mt.closeCh
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	srv.Close()
	wg.Wait()
}

// ── HubShutdown ReadPump inline cleanup ─────────────────────────────────────

func TestComponent_HubShutdown_ReadPumpInlineCleanup(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
	)

	_ = injectAndWait(t, srv, "test-connection", "test-room", connected)

	// Close the server — hub shuts down, readPump should do inline cleanup.
	srv.Close()
}
