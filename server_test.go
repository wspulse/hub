package server_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/goleak"
	"go.uber.org/zap/zaptest"

	wspulse "github.com/wspulse/server"
)

func dialTestServer(t *testing.T, srv wspulse.Server) *websocket.Conn {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func acceptAll(r *http.Request) (roomID, connectionID string, err error) {
	return "test-room", "test-connection", nil
}

func TestServer_Send_ErrConnectionNotFound(t *testing.T) {
	srv := wspulse.NewServer(acceptAll)
	t.Cleanup(srv.Close)
	err := srv.Send("does-not-exist", wspulse.Frame{Event: "ping"})
	if !errors.Is(err, wspulse.ErrConnectionNotFound) {
		t.Fatalf("want ErrConnectionNotFound, got %v", err)
	}
}

func TestServer_Kick_ErrConnectionNotFound(t *testing.T) {
	srv := wspulse.NewServer(acceptAll)
	t.Cleanup(srv.Close)
	err := srv.Kick("does-not-exist")
	if !errors.Is(err, wspulse.ErrConnectionNotFound) {
		t.Fatalf("want ErrConnectionNotFound, got %v", err)
	}
}

func TestServer_GetConnections_UnknownRoom_ReturnsEmptySlice(t *testing.T) {
	srv := wspulse.NewServer(acceptAll)
	t.Cleanup(srv.Close)
	connections := srv.GetConnections("no-such-room")
	if len(connections) != 0 {
		t.Fatalf("want 0 connections, got %d", len(connections))
	}
}

func TestServer_ConnectFunc_RejectReturns401(t *testing.T) {
	srv := wspulse.NewServer(func(r *http.Request) (string, string, error) {
		return "", "", errors.New("unauthorized")
	})
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

func TestServer_OnConnect_SendsFrame(t *testing.T) {
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			_ = connection.Send(wspulse.Frame{Event: "welcome", Payload: []byte(`"hello"`)})
		}),
	)
	t.Cleanup(srv.Close)
	c := dialTestServer(t, srv)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	f, err := wspulse.JSONCodec.Decode(message)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if f.Event != "welcome" {
		t.Errorf("Event: want %q, got %q", "welcome", f.Event)
	}
}

func TestServer_OnMessage_CallbackFires(t *testing.T) {
	received := make(chan wspulse.Frame, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnMessage(func(connection wspulse.Connection, f wspulse.Frame) {
			received <- f
		}),
	)
	t.Cleanup(srv.Close)
	c := dialTestServer(t, srv)
	payload := []byte(`{"text":"ping from client"}`)
	encoded, _ := wspulse.JSONCodec.Encode(wspulse.Frame{Event: "msg", Payload: payload})
	if err := c.WriteMessage(websocket.TextMessage, encoded); err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}
	select {
	case f := <-received:
		if f.Event != "msg" {
			t.Errorf("Event: want %q, got %q", "msg", f.Event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnMessage callback")
	}
}

func TestServer_Broadcast_ReachesConnectedClient(t *testing.T) {
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)
	c := dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connection to register")
	}
	frame := wspulse.Frame{Event: "notice", Payload: []byte(`"hello room"`)}
	if err := srv.Broadcast("test-room", frame); err != nil {
		t.Fatalf("Broadcast failed: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	f, err := wspulse.JSONCodec.Decode(message)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if f.Event != "notice" {
		t.Errorf("Event: want %q, got %q", "notice", f.Event)
	}
}

func TestServer_OnDisconnect_CallbackFires(t *testing.T) {
	disconnected := make(chan struct{})
	var once sync.Once
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			once.Do(func() { close(disconnected) })
		}),
	)
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	_ = c.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
	_ = c.Close()
	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnDisconnect callback")
	}
}

func TestServer_Close_SafeToCallTwice(t *testing.T) {
	srv := wspulse.NewServer(acceptAll)
	srv.Close() // first call
	srv.Close() // must not panic
}

func TestServer_Send_DeliversFrameToConnection(t *testing.T) {
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)
	c := dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connection to register")
	}
	frame := wspulse.Frame{Event: "direct", Payload: []byte(`"hi"`)}
	if err := srv.Send("test-connection", frame); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	f, err := wspulse.JSONCodec.Decode(message)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if f.Event != "direct" {
		t.Errorf("Event: want %q, got %q", "direct", f.Event)
	}
}

func TestServer_Kick_ClosesConnection(t *testing.T) {
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{})
	var once sync.Once
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			once.Do(func() { close(disconnected) })
		}),
	)
	t.Cleanup(srv.Close)
	_ = dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connection to register")
	}
	if err := srv.Kick("test-connection"); err != nil {
		t.Fatalf("Kick failed: %v", err)
	}
	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for disconnection after Kick")
	}
}

func TestWithCodec_Nil_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil codec")
		}
	}()
	_ = wspulse.WithCodec(nil)
}

func TestWithHeartbeat_InvalidParams_Panics(t *testing.T) {
	cases := []struct {
		name       string
		ping, pong time.Duration
	}{
		{"ping == pong", 10 * time.Second, 10 * time.Second},
		{"ping > pong", 30 * time.Second, 10 * time.Second},
		{"ping zero", 0, 10 * time.Second},
		{"pong zero", 10 * time.Second, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected panic for pingPeriod=%v pongWait=%v", tc.ping, tc.pong)
				}
			}()
			_ = wspulse.WithHeartbeat(tc.ping, tc.pong)
		})
	}
}

func TestServer_GetConnections_ReturnsRegisteredConnection(t *testing.T) {
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)
	_ = dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connection to register")
	}
	connections := srv.GetConnections("test-room")
	if len(connections) != 1 {
		t.Fatalf("want 1 connection in test-room, got %d", len(connections))
	}
	if connections[0].ID() != "test-connection" {
		t.Errorf("connection ID: want %q, got %q", "test-connection", connections[0].ID())
	}
	if connections[0].RoomID() != "test-room" {
		t.Errorf("connection RoomID: want %q, got %q", "test-room", connections[0].RoomID())
	}
}

func TestServer_DuplicateConnectionID_OldKickedNewReachable(t *testing.T) {
	var (
		firstConnected  = make(chan struct{})
		secondConnected = make(chan struct{})
		kicked          = make(chan struct{})
		mu              sync.Mutex
		connectionCount int
		disconnectCount int
	)
	srv := wspulse.NewServer(
		acceptAll, // always returns connectionID="test-connection"
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			mu.Lock()
			connectionCount++
			n := connectionCount
			mu.Unlock()
			if n == 1 {
				close(firstConnected)
			} else {
				close(secondConnected)
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			mu.Lock()
			disconnectCount++
			mu.Unlock()
			if errors.Is(err, wspulse.ErrDuplicateConnectionID) {
				close(kicked)
			}
		}),
	)
	t.Cleanup(srv.Close)

	// Establish first connection.
	_ = dialTestServer(t, srv)
	select {
	case <-firstConnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first connection")
	}

	// Establish second connection with the same connectionID — first must be kicked.
	c2 := dialTestServer(t, srv)
	select {
	case <-kicked:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for ErrDuplicateConnectionID kick")
	}
	select {
	case <-secondConnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for second connection to be registered")
	}

	// Give any spurious second onDisconnect call a chance to arrive.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	dc := disconnectCount
	mu.Unlock()
	if dc != 1 {
		t.Errorf("onDisconnect called %d times for kicked connection, want exactly 1", dc)
	}

	// Server.Send must reach the new connection, not return ErrConnectionNotFound.
	frame := wspulse.Frame{Event: "ok", Payload: []byte(`"after-kick"`)}
	if err := srv.Send("test-connection", frame); err != nil {
		t.Fatalf("Send to second connection failed: %v", err)
	}
	_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage on second connection failed: %v", err)
	}
	f, err := wspulse.JSONCodec.Decode(message)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if f.Event != "ok" {
		t.Errorf("Event: want %q, got %q", "ok", f.Event)
	}
}

func TestNewServer_NilConnect_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil ConnectFunc")
		}
	}()
	_ = wspulse.NewServer(nil)
}

func TestWithLogger_Nil_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil logger")
		}
	}()
	_ = wspulse.WithLogger(nil)
}

func TestWithMaxMessageSize_Zero_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for zero max message size")
		}
	}()
	_ = wspulse.WithMaxMessageSize(0)
}

// ── Race & backpressure tests ─────────────────────────────────────────────────

func TestServer_ConcurrentBroadcast_NoRace(t *testing.T) {
	const workers = 8
	const messagesPerWorker = 50

	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "room", "", nil // random connectionID each time
		},
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	// Establish a connection so the room exists.
	_ = dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connection")
	}

	var wg sync.WaitGroup
	// Concurrent broadcasters.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < messagesPerWorker; j++ {
				_ = srv.Broadcast("room", wspulse.Frame{Event: "ping"})
			}
		}()
	}
	// Concurrent connector/disconnector.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			ts := httptest.NewServer(srv)
			u := "ws" + strings.TrimPrefix(ts.URL, "http")
			dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
			c, _, err := dialer.Dial(u, nil)
			if err == nil {
				_ = c.Close()
			}
			ts.Close()
		}
	}()
	wg.Wait()
}

func TestServer_CloseWhileConnecting_NoLeak(t *testing.T) {
	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "room", "", nil
		},
	)

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	u := "ws" + strings.TrimPrefix(ts.URL, "http")

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
			c, _, err := dialer.Dial(u, nil)
			if err == nil {
				for {
					if _, _, readErr := c.ReadMessage(); readErr != nil {
						break
					}
				}
				_ = c.Close()
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	srv.Close()
	wg.Wait()
}

func TestServer_BroadcastDropsOldest_SlowClient(t *testing.T) {
	const bufferSize = 2
	const totalBroadcasts = 200

	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithSendBufferSize(bufferSize),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	c := dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connection")
	}

	for i := 0; i < totalBroadcasts; i++ {
		typ := "old"
		if i == totalBroadcasts-1 {
			typ = "newest"
		}
		_ = srv.Broadcast("test-room", wspulse.Frame{Event: typ, Payload: []byte(`"x"`)})
	}

	time.Sleep(300 * time.Millisecond)

	var frames []wspulse.Frame
	for {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, message, err := c.ReadMessage()
		if err != nil {
			break
		}
		f, decodeErr := wspulse.JSONCodec.Decode(message)
		if decodeErr != nil {
			t.Fatalf("Decode failed: %v", decodeErr)
		}
		frames = append(frames, f)
	}

	if len(frames) == 0 {
		t.Fatal("expected at least one frame, got none")
	}

	found := false
	for _, f := range frames {
		if f.Event == "newest" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("newest frame not found among %d received frames; drop-oldest policy did not preserve it", len(frames))
	}
}

func TestServer_ShutdownFiresOnDisconnect(t *testing.T) {
	const connectionCount = 3
	var mu sync.Mutex
	disconnected := make(map[string]error)
	allDone := make(chan struct{})

	connectionIndex := 0
	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			mu.Lock()
			id := connectionIndex
			connectionIndex++
			mu.Unlock()
			return "room", "connection-" + strings.Repeat("x", id), nil
		},
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			mu.Lock()
			disconnected[connection.ID()] = err
			if len(disconnected) == connectionCount {
				select {
				case <-allDone:
				default:
					close(allDone)
				}
			}
			mu.Unlock()
		}),
	)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	for i := 0; i < connectionCount; i++ {
		c, _, err := websocket.DefaultDialer.Dial(u, nil)
		if err != nil {
			t.Fatalf("Dial %d failed: %v", i, err)
		}
		defer c.Close() //nolint:errcheck
	}

	time.Sleep(200 * time.Millisecond)
	srv.Close()

	select {
	case <-allDone:
	case <-time.After(3 * time.Second):
		mu.Lock()
		t.Fatalf("timed out: only %d/%d OnDisconnect callbacks fired", len(disconnected), connectionCount)
		mu.Unlock()
	}

	mu.Lock()
	defer mu.Unlock()
	for id, err := range disconnected {
		if !errors.Is(err, wspulse.ErrServerClosed) {
			t.Errorf("connection %s: got err=%v, want ErrServerClosed", id, err)
		}
	}
}

func TestServer_ReadPumpPanicRecovery(t *testing.T) {
	disconnected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnMessage(func(connection wspulse.Connection, f wspulse.Frame) {
			panic("boom")
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)
	c := dialTestServer(t, srv)

	data, _ := wspulse.JSONCodec.Encode(wspulse.Frame{Event: "trigger"})
	_ = c.WriteMessage(websocket.TextMessage, data)

	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnDisconnect after panic")
	}
}

func TestServer_Broadcast_AfterClose_ReturnsErrServerClosed(t *testing.T) {
	srv := wspulse.NewServer(acceptAll)
	srv.Close()
	err := srv.Broadcast("test-room", wspulse.Frame{Event: "msg"})
	if !errors.Is(err, wspulse.ErrServerClosed) {
		t.Fatalf("want ErrServerClosed, got %v", err)
	}
}

func TestServer_ConnectionSend_BufferFull_ReturnsErrSendBufferFull(t *testing.T) {
	connected := make(chan wspulse.Connection, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			connected <- connection
		}),
		wspulse.WithSendBufferSize(1),
	)
	t.Cleanup(srv.Close)
	c := dialTestServer(t, srv)
	_ = c // keep alive

	var connection wspulse.Connection
	select {
	case connection = <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out: never saw ErrSendBufferFull")
		default:
		}
		err := connection.Send(wspulse.Frame{Event: "flood"})
		if errors.Is(err, wspulse.ErrSendBufferFull) {
			return // success
		}
		if errors.Is(err, wspulse.ErrConnectionClosed) {
			t.Fatal("connection closed before buffer-full was observed")
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func TestServer_ConnectionDone_ClosedOnKick(t *testing.T) {
	connected := make(chan wspulse.Connection, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			connected <- connection
		}),
	)
	t.Cleanup(srv.Close)
	c := dialTestServer(t, srv)
	_ = c

	var connection wspulse.Connection
	select {
	case connection = <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	_ = srv.Kick(connection.ID())

	select {
	case <-connection.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Connection.Done()")
	}
}

func TestServer_MultipleRooms_BroadcastIsolation(t *testing.T) {
	connectionIndex := 0
	var mu sync.Mutex
	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			mu.Lock()
			defer mu.Unlock()
			connectionIndex++
			if connectionIndex == 1 {
				return "room-a", "connection-a", nil
			}
			return "room-b", "connection-b", nil
		},
		wspulse.WithOnConnect(func(connection wspulse.Connection) {}),
	)
	t.Cleanup(srv.Close)

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	u := "ws" + strings.TrimPrefix(ts.URL, "http")

	cA, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("Dial A failed: %v", err)
	}
	t.Cleanup(func() { _ = cA.Close() })

	cB, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("Dial B failed: %v", err)
	}
	t.Cleanup(func() { _ = cB.Close() })

	time.Sleep(100 * time.Millisecond)

	if err := srv.Broadcast("room-a", wspulse.Frame{Event: "hello"}); err != nil {
		t.Fatalf("Broadcast failed: %v", err)
	}

	_ = cA.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, errA := cA.ReadMessage()
	if errA != nil {
		t.Fatalf("room-a client didn't receive frame: %v", errA)
	}

	_ = cB.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, errB := cB.ReadMessage()
	if errB == nil {
		t.Fatal("room-b client received a frame intended for room-a")
	}
}

func TestServer_GetConnections_EmptyAfterDisconnect(t *testing.T) {
	disconnected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)
	c := dialTestServer(t, srv)

	_ = c.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
	_ = c.Close()

	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for disconnect")
	}

	time.Sleep(50 * time.Millisecond)
	connections := srv.GetConnections("test-room")
	if len(connections) != 0 {
		t.Fatalf("want 0 connections after disconnect, got %d", len(connections))
	}
}

// ── Session Resumption tests ──────────────────────────────────────────────────

func dialTestServerRaw(t *testing.T, srv wspulse.Server) (*websocket.Conn, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	return c, ts
}

func TestServer_Resume_ReconnectWithinWindow(t *testing.T) {
	disconnected := make(chan struct{}, 1)
	connected := make(chan struct{}, 2)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	c1, ts := dialTestServerRaw(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first connect")
	}

	_ = c1.Close()
	time.Sleep(200 * time.Millisecond)

	select {
	case <-disconnected:
		t.Fatal("OnDisconnect fired during resume window — should not happen")
	default:
	}

	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c2, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("reconnect dial failed: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })

	time.Sleep(200 * time.Millisecond)

	frame := wspulse.Frame{Event: "after-resume", Payload: []byte(`"ok"`)}
	if err := srv.Send("test-connection", frame); err != nil {
		t.Fatalf("Send after resume failed: %v", err)
	}
	_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage on resumed connection failed: %v", err)
	}
	f, _ := wspulse.JSONCodec.Decode(message)
	if f.Event != "after-resume" {
		t.Errorf("Event: want %q, got %q", "after-resume", f.Event)
	}

	select {
	case <-disconnected:
		t.Fatal("OnDisconnect fired after successful resume")
	default:
	}
}

func TestServer_Resume_GraceExpires_FiresOnDisconnect(t *testing.T) {
	disconnected := make(chan struct{}, 1)
	connected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(1),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	c := dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	_ = c.Close()

	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnDisconnect after grace expiry")
	}
}

func TestServer_Resume_BufferedFramesDelivered(t *testing.T) {
	connected := make(chan struct{}, 2)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	c1, ts := dialTestServerRaw(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first connect")
	}

	_ = c1.Close()
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < 3; i++ {
		_ = srv.Send("test-connection", wspulse.Frame{
			Event:   "buffered",
			Payload: []byte(`"` + string(rune('a'+i)) + `"`),
		})
	}

	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c2, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("reconnect dial failed: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })

	var frames []wspulse.Frame
	for i := 0; i < 3; i++ {
		_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, message, err := c2.ReadMessage()
		if err != nil {
			t.Fatalf("ReadMessage %d failed: %v", i, err)
		}
		f, _ := wspulse.JSONCodec.Decode(message)
		frames = append(frames, f)
	}

	if len(frames) != 3 {
		t.Fatalf("want 3 buffered frames, got %d", len(frames))
	}
	for _, f := range frames {
		if f.Event != "buffered" {
			t.Errorf("Event: want %q, got %q", "buffered", f.Event)
		}
	}
}

func TestServer_Resume_KickBypassesWindow(t *testing.T) {
	disconnected := make(chan struct{}, 1)
	connected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(10),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	_ = dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	if err := srv.Kick("test-connection"); err != nil {
		t.Fatalf("Kick failed: %v", err)
	}

	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnDisconnect after Kick (should bypass resume window)")
	}
}

func TestServer_Resume_NoResumeWindow_DisconnectsImmediately(t *testing.T) {
	disconnected := make(chan struct{}, 1)
	connected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(0),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	c := dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	_ = c.Close()

	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnDisconnect (no resume window)")
	}
}

func TestServer_Resume_ServerCloseTerminatesSuspended(t *testing.T) {
	disconnected := make(chan struct{}, 1)
	connected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(10),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)

	c := dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	_ = c.Close()
	time.Sleep(200 * time.Millisecond)

	srv.Close()

	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnDisconnect from Server.Close on suspended session")
	}
}

func TestServer_Resume_BroadcastWhileSuspended(t *testing.T) {
	connected := make(chan struct{}, 2)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	c1, ts := dialTestServerRaw(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	_ = c1.Close()
	time.Sleep(200 * time.Millisecond)

	_ = srv.Broadcast("test-room", wspulse.Frame{Event: "bcast", Payload: []byte(`"suspended"`)})

	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c2, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("reconnect dial failed: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })

	_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	f, _ := wspulse.JSONCodec.Decode(message)
	if f.Event != "bcast" {
		t.Errorf("Event: want %q, got %q", "bcast", f.Event)
	}
}

// ── Race condition tests for resume ───────────────────────────────────────────

func TestServer_Resume_ConcurrentReconnect_NoRace(t *testing.T) {
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(2),
	)
	t.Cleanup(srv.Close)

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	u := "ws" + strings.TrimPrefix(ts.URL, "http")

	for i := 0; i < 10; i++ {
		dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
		c, _, err := dialer.Dial(u, nil)
		if err != nil {
			continue
		}
		time.Sleep(10 * time.Millisecond)
		_ = c.Close()
		time.Sleep(50 * time.Millisecond)
	}
}

func TestServer_Resume_ConcurrentBroadcastDuringResume_NoRace(t *testing.T) {
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(2),
	)
	t.Cleanup(srv.Close)

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	u := "ws" + strings.TrimPrefix(ts.URL, "http")

	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	c, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = srv.Broadcast("test-room", wspulse.Frame{Event: "ping"})
				time.Sleep(time.Millisecond)
			}
		}()
	}

	time.Sleep(20 * time.Millisecond)
	_ = c.Close()

	time.Sleep(50 * time.Millisecond)
	c2, _, err := dialer.Dial(u, nil)
	if err == nil {
		t.Cleanup(func() { _ = c2.Close() })
	}

	wg.Wait()
}

// ── Option validation tests ───────────────────────────────────────────────────

func TestWithHeartbeat_ValidParams_Accepted(t *testing.T) {
	srv := wspulse.NewServer(acceptAll,
		wspulse.WithHeartbeat(5*time.Second, 15*time.Second),
	)
	t.Cleanup(srv.Close)
}

func TestWithHeartbeat_PingExceedsMax_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for pingPeriod > 5m")
		}
	}()
	_ = wspulse.WithHeartbeat(6*time.Minute, 10*time.Minute)
}

func TestWithHeartbeat_PongExceedsMax_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for pongWait > 10m")
		}
	}()
	_ = wspulse.WithHeartbeat(1*time.Minute, 11*time.Minute)
}

func TestWithWriteWait_ValidDuration_Accepted(t *testing.T) {
	srv := wspulse.NewServer(acceptAll,
		wspulse.WithWriteWait(5*time.Second),
	)
	t.Cleanup(srv.Close)
}

func TestWithWriteWait_Zero_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for zero write wait")
		}
	}()
	_ = wspulse.WithWriteWait(0)
}

func TestWithWriteWait_ExceedsMax_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for write wait > 30s")
		}
	}()
	_ = wspulse.WithWriteWait(31 * time.Second)
}

func TestWithMaxMessageSize_ValidSize_Accepted(t *testing.T) {
	srv := wspulse.NewServer(acceptAll,
		wspulse.WithMaxMessageSize(4096),
	)
	t.Cleanup(srv.Close)
}

func TestWithMaxMessageSize_ExceedsMax_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for size > 64 MiB")
		}
	}()
	_ = wspulse.WithMaxMessageSize(64<<20 + 1)
}

func TestWithSendBufferSize_Zero_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for zero buffer size")
		}
	}()
	_ = wspulse.WithSendBufferSize(0)
}

func TestWithSendBufferSize_ExceedsMax_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for buffer size > 4096")
		}
	}()
	_ = wspulse.WithSendBufferSize(4097)
}

func TestWithCheckOrigin_Nil_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil CheckOrigin")
		}
	}()
	_ = wspulse.WithCheckOrigin(nil)
}

func TestWithCheckOrigin_RejectsConnection(t *testing.T) {
	srv := wspulse.NewServer(acceptAll,
		wspulse.WithCheckOrigin(func(r *http.Request) bool { return false }),
	)
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	_, _, err := dialer.Dial(u, nil)
	if err == nil {
		t.Fatal("expected dial to fail when origin is rejected")
	}
}

func TestWithResumeWindow_Negative_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for negative resume window")
		}
	}()
	_ = wspulse.WithResumeWindow(-1)
}

func TestWithResumeWindow_ExceedsMax_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for resume window > 180")
		}
	}()
	_ = wspulse.WithResumeWindow(181)
}

func TestWithCodec_ValidCodec_Accepted(t *testing.T) {
	srv := wspulse.NewServer(acceptAll,
		wspulse.WithCodec(wspulse.JSONCodec),
	)
	t.Cleanup(srv.Close)
}

func TestWithLogger_ValidLogger_Accepted(t *testing.T) {
	srv := wspulse.NewServer(acceptAll,
		wspulse.WithLogger(zaptest.NewLogger(t)),
	)
	t.Cleanup(srv.Close)
}

// ── ServeHTTP edge case tests ─────────────────────────────────────────────────

func TestServer_ServeHTTP_AfterClose_Returns503(t *testing.T) {
	srv := wspulse.NewServer(acceptAll)
	srv.Close()

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("want 503, got %d", resp.StatusCode)
	}
}

func TestServer_ServeHTTP_EmptyConnectionID_GetsUUID(t *testing.T) {
	connected := make(chan wspulse.Connection, 1)
	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "room", "", nil // empty connectionID → auto UUID
		},
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			connected <- connection
		}),
	)
	t.Cleanup(srv.Close)
	_ = dialTestServer(t, srv)

	select {
	case c := <-connected:
		if c.ID() == "" {
			t.Error("expected non-empty auto-generated connectionID")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}
}

// ── Session Send edge case tests ──────────────────────────────────────────────

func TestServer_ConnectionSend_AfterClose_ReturnsErrConnectionClosed(t *testing.T) {
	connected := make(chan wspulse.Connection, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			connected <- connection
		}),
	)
	t.Cleanup(srv.Close)
	_ = dialTestServer(t, srv)

	var connection wspulse.Connection
	select {
	case connection = <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	_ = srv.Kick(connection.ID())
	<-connection.Done()

	err := connection.Send(wspulse.Frame{Event: "nope"})
	if !errors.Is(err, wspulse.ErrConnectionClosed) {
		t.Fatalf("want ErrConnectionClosed, got %v", err)
	}
}

// ── Broadcast edge case tests ─────────────────────────────────────────────────

func TestServer_Broadcast_SkipsClosedSession(t *testing.T) {
	// Broadcast while a connection is being kicked should not panic.
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)
	_ = dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out")
	}

	_ = srv.Kick("test-connection")
	// Broadcast after kick — the session is closed and should be skipped.
	_ = srv.Broadcast("test-room", wspulse.Frame{Event: "after-kick"})
}

// ── readPump edge case tests ──────────────────────────────────────────────────

func TestServer_ReadPump_MalformedFrame_DropsAndContinues(t *testing.T) {
	received := make(chan wspulse.Frame, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnMessage(func(connection wspulse.Connection, f wspulse.Frame) {
			received <- f
		}),
		wspulse.WithMaxMessageSize(1024),
	)
	t.Cleanup(srv.Close)
	c := dialTestServer(t, srv)

	// Send malformed JSON (not a valid Frame).
	if err := c.WriteMessage(websocket.TextMessage, []byte("not json")); err != nil {
		t.Fatalf("WriteMessage (malformed) failed: %v", err)
	}
	// Send a valid frame after the malformed one — readPump should continue.
	validData, _ := wspulse.JSONCodec.Encode(wspulse.Frame{Event: "valid"})
	if err := c.WriteMessage(websocket.TextMessage, validData); err != nil {
		t.Fatalf("WriteMessage (valid) failed: %v", err)
	}

	select {
	case f := <-received:
		if f.Event != "valid" {
			t.Errorf("Event: want %q, got %q", "valid", f.Event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for valid frame after malformed one")
	}
}

// ── Kick and Broadcast during server shutdown ─────────────────────────────────

func TestServer_Kick_AfterClose_ReturnsErrServerClosed(t *testing.T) {
	srv := wspulse.NewServer(acceptAll)
	srv.Close()
	if err := srv.Kick("any"); !errors.Is(err, wspulse.ErrServerClosed) {
		t.Fatalf("want ErrServerClosed, got %v", err)
	}
}

// ── Resume: stale closed session in register ─────────────────────────────────

func TestServer_Resume_StaleClosedSession_Reconnect(t *testing.T) {
	// Trigger the stateClosed path in handleRegister: connect, let grace
	// expire (session becomes closed), then reconnect with the same ID.
	connected := make(chan struct{}, 4)
	disconnected := make(chan struct{}, 4)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(1), // 1-second window
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	c1, ts := dialTestServerRaw(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first connect")
	}

	// Close transport and wait for grace to expire.
	_ = c1.Close()
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for grace expiry disconnect")
	}

	// Reconnect with the same connectionID — hits stateClosed branch.
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c2, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("reconnect dial failed: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })

	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reconnect after stale session cleanup")
	}

	// Verify the new connection works.
	if err := srv.Send("test-connection", wspulse.Frame{Event: "alive"}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	f, _ := wspulse.JSONCodec.Decode(message)
	if f.Event != "alive" {
		t.Errorf("Event: want %q, got %q", "alive", f.Event)
	}
}

// ── Server.Close() synchronous behavior ───────────────────────────────────────

func TestServer_Close_BlocksUntilHubExits(t *testing.T) {
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	_ = dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Close should block until hub goroutine is done.
	done := make(chan struct{})
	go func() {
		srv.Close()
		close(done)
	}()
	select {
	case <-done:
		// After Close returns, a second Close must return immediately.
		srv.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not return within timeout")
	}
}

// ── Concurrent Close and Kick ─────────────────────────────────────────────────

func TestServer_ConcurrentCloseAndKick_NoRace(t *testing.T) {
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	_ = dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

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

// ── Concurrent Close and Broadcast ────────────────────────────────────────────

func TestServer_ConcurrentCloseAndBroadcast_NoRace(t *testing.T) {
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	_ = dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		srv.Close()
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = srv.Broadcast("test-room", wspulse.Frame{Event: "msg"})
		}
	}()
	wg.Wait()
}

// ── Resume: connection.Close() while suspended ───────────────────────────────

func TestServer_Resume_ConnectionCloseWhileSuspended(t *testing.T) {
	connected := make(chan wspulse.Connection, 1)
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(10),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- connection:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	c := dialTestServer(t, srv)
	var connection wspulse.Connection
	select {
	case connection = <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Close the transport to trigger suspend.
	_ = c.Close()
	time.Sleep(200 * time.Millisecond)

	// Application calls Close() on the Connection while suspended.
	_ = connection.Close()

	// Wait for Done() channel to be closed.
	select {
	case <-connection.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done() not closed after Close()")
	}
}

// ── Multiple rapid resume cycles ──────────────────────────────────────────────

func TestServer_Resume_MultipleRapidCycles(t *testing.T) {
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5),
	)
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}

	for i := 0; i < 5; i++ {
		c, _, err := dialer.Dial(u, nil)
		if err != nil {
			t.Fatalf("cycle %d: dial failed: %v", i, err)
		}
		time.Sleep(50 * time.Millisecond)
		_ = c.Close()
		time.Sleep(100 * time.Millisecond) // within resume window
	}
}

// ── Broadcast to empty or unknown room (already partially covered) ────────────

func TestServer_Broadcast_EmptyRoom_NoError(t *testing.T) {
	srv := wspulse.NewServer(acceptAll)
	t.Cleanup(srv.Close)
	err := srv.Broadcast("nonexistent-room", wspulse.Frame{Event: "msg"})
	if err != nil {
		t.Fatalf("expected no error for empty room, got %v", err)
	}
}

// ── Codec error path tests ───────────────────────────────────────────────────

// failingCodec is a Codec that always returns an error on Encode.
type failingCodec struct{}

func (failingCodec) Encode(wspulse.Frame) ([]byte, error) {
	return nil, errors.New("codec: encode failed")
}

func (failingCodec) Decode(data []byte) (wspulse.Frame, error) {
	return wspulse.JSONCodec.Decode(data)
}

func (failingCodec) FrameType() int { return wspulse.TextMessage }

func TestServer_Broadcast_EncodeError_ReturnsError(t *testing.T) {
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithCodec(failingCodec{}),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)
	_ = dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	err := srv.Broadcast("test-room", wspulse.Frame{Event: "fail"})
	if err == nil {
		t.Fatal("expected encode error from Broadcast")
	}
}

func TestServer_ConnectionSend_EncodeError_ReturnsError(t *testing.T) {
	connected := make(chan wspulse.Connection, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithCodec(failingCodec{}),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			connected <- connection
		}),
	)
	t.Cleanup(srv.Close)
	_ = dialTestServer(t, srv)

	var connection wspulse.Connection
	select {
	case connection = <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	err := connection.Send(wspulse.Frame{Event: "fail"})
	if err == nil {
		t.Fatal("expected encode error from Send")
	}
}

// ── Grace expired for already-closed session ─────────────────────────────────

func TestServer_Resume_GraceExpiresAfterConnectionClose(t *testing.T) {
	// Tests the handleGraceExpired stateClosed path: connection.Close()
	// is called while suspended, then the grace timer fires.
	connected := make(chan wspulse.Connection, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(1),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- connection:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	c := dialTestServer(t, srv)
	var connection wspulse.Connection
	select {
	case connection = <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Close transport → session suspends.
	_ = c.Close()
	time.Sleep(200 * time.Millisecond)

	// Application calls Close() on the Connection while suspended.
	_ = connection.Close()

	// Wait for grace timer to fire (1 second). handleGraceExpired will see
	// stateClosed and clean up maps without calling Close again.
	time.Sleep(1500 * time.Millisecond)

	// Verify session is fully cleaned up.
	connections := srv.GetConnections("test-room")
	if len(connections) != 0 {
		t.Fatalf("want 0 connections after grace expired, got %d", len(connections))
	}
}

// ── Resume: stale grace timer (epoch mismatch) ──────────────────────────────

func TestServer_Resume_StaleGraceTimerIgnored(t *testing.T) {
	// Connect, disconnect (suspend, timer start), reconnect (resume, epoch bump),
	// disconnect again (new timer), let OLD timer fire (epoch mismatch → ignored),
	// then reconnect again — session must still be alive.
	connected := make(chan struct{}, 10)
	disconnected := make(chan struct{}, 10)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(1),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	// Cycle 1: connect → disconnect (suspend).
	c1, ts := dialTestServerRaw(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first connect")
	}
	_ = c1.Close()
	time.Sleep(200 * time.Millisecond) // grace timer epoch=1 started

	// Cycle 2: reconnect (resume, epoch bumped), disconnect again.
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c2, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("reconnect 1 failed: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	_ = c2.Close()
	time.Sleep(200 * time.Millisecond) // grace timer epoch=2 started

	// Cycle 3: reconnect again (resume, epoch bumped again).
	c3, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("reconnect 2 failed: %v", err)
	}
	t.Cleanup(func() { _ = c3.Close() })

	// Wait for the old timers (epoch=1, epoch=2) to fire — both should be
	// detected as stale and ignored.
	time.Sleep(1200 * time.Millisecond)

	// Session should still be alive.
	select {
	case <-disconnected:
		t.Fatal("OnDisconnect fired — stale timer was not correctly ignored")
	default:
	}

	if err := srv.Send("test-connection", wspulse.Frame{Event: "alive"}); err != nil {
		t.Fatalf("Send after stale timers failed: %v", err)
	}
}

// ── handleTransportDied: transport-died for closed session (kick then readPump reports) ─

func TestServer_Resume_KickWhileConnected_TransportDiedHandled(t *testing.T) {
	// When Kick is called on a connected session with resume enabled,
	// the session transitions to stateClosed. Later when readPump exits
	// (transport closed by writePump's defer), the transportDiedMessage
	// arrives at the hub with state == stateClosed.
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(10),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)
	_ = dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	if err := srv.Kick("test-connection"); err != nil {
		t.Fatalf("Kick failed: %v", err)
	}

	// After kick, the transportDied message from readPump arrives with
	// stateClosed. Wait for everything to settle.
	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OnDisconnect")
	}
	time.Sleep(200 * time.Millisecond) // let deferred cleanup finish
}

// ── readPump and writePump: inline hub-shutdown cleanup ──────────────────────

func TestServer_HubShutdown_ReadPumpInlineCleanup(t *testing.T) {
	// When the hub is closed while connections are active, readPumps that
	// discover h.done is closed should call s.closeOnce.Do inline.
	const count = 5
	var mu sync.Mutex
	var connectedCount int
	allConnected := make(chan struct{})

	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			mu.Lock()
			defer mu.Unlock()
			connectedCount++
			return "room", "", nil // auto UUID
		},
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			mu.Lock()
			n := connectedCount
			mu.Unlock()
			if n >= count {
				select {
				case <-allConnected:
				default:
					close(allConnected)
				}
			}
		}),
	)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	clients := make([]*websocket.Conn, 0, count)
	for i := 0; i < count; i++ {
		c, _, err := websocket.DefaultDialer.Dial(u, nil)
		if err != nil {
			t.Fatalf("Dial %d failed: %v", i, err)
		}
		clients = append(clients, c)
	}
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()

	select {
	case <-allConnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for all connections")
	}

	// Close the server — hub shuts down, readPumps should cleanup inline.
	srv.Close()
}

// ── Send/Kick during hub close (both ErrServerClosed paths) ──────────────────

func TestServer_Send_AfterClose_ReturnsErrConnectionNotFound(t *testing.T) {
	srv := wspulse.NewServer(acceptAll)
	srv.Close()
	// After close, hub maps are empty — returns ErrConnectionNotFound.
	err := srv.Send("any", wspulse.Frame{Event: "x"})
	if !errors.Is(err, wspulse.ErrConnectionNotFound) {
		t.Fatalf("want ErrConnectionNotFound, got %v", err)
	}
}

// ── Duplicate connection with resume enabled ─────────────────────────────────

func TestServer_Resume_DuplicateID_WhileConnected_KicksOld(t *testing.T) {
	var (
		firstConnected  = make(chan struct{})
		kicked          = make(chan struct{})
		mu              sync.Mutex
		connectionCount int
	)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			mu.Lock()
			connectionCount++
			n := connectionCount
			mu.Unlock()
			if n == 1 {
				close(firstConnected)
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			if errors.Is(err, wspulse.ErrDuplicateConnectionID) {
				close(kicked)
			}
		}),
	)
	t.Cleanup(srv.Close)

	_ = dialTestServer(t, srv)
	select {
	case <-firstConnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first connection")
	}

	// Second connection with same ID while first is still connected (not suspended).
	_ = dialTestServer(t, srv)
	select {
	case <-kicked:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for duplicate kick")
	}
}

// ── Pong handler coverage ────────────────────────────────────────────────────

func TestServer_PongHandler_ResetsReadDeadline(t *testing.T) {
	// Use a short ping period so the server sends a Ping quickly.
	// The gorilla client auto-responds with Pong, which fires the
	// PongHandler on the server and covers that code path.
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithHeartbeat(200*time.Millisecond, 2*time.Second),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)
	c := dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Keep reading for long enough to receive at least one Ping+Pong cycle.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err != nil {
			break
		}
	}
}

// ── Normal close frame (covers "connection closed normally" log) ─────────────

func TestServer_NormalCloseFrame_LogsNormally(t *testing.T) {
	disconnected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}

	// Send a proper close frame and wait for the server to process it.
	_ = c.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseGoingAway, "bye"))
	// Read the close response before closing the TCP connection.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, _ = c.ReadMessage() // read close frame response
	_ = c.Close()

	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for disconnect")
	}
}

// ── writePump pumpQuit: verify pump stops on transport swap ──────────────────

func TestServer_Resume_WritePumpStopsOnPumpQuit(t *testing.T) {
	// Verifies that writePump exits via pumpQuit when the session is suspended.
	// After suspend + resume, the old writePump must have exited (via pumpQuit),
	// because the new writePump successfully delivers frames.
	connected := make(chan struct{}, 2)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5),
		wspulse.WithHeartbeat(100*time.Millisecond, 1*time.Second),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	c1, ts := dialTestServerRaw(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first connect")
	}

	// Send data before closing to ensure writePump is active.
	_ = srv.Send("test-connection", wspulse.Frame{Event: "pre-suspend"})
	_ = c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := c1.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}

	// Close transport → session suspends, writePump gets pumpQuit.
	_ = c1.Close()
	time.Sleep(300 * time.Millisecond)

	// Reconnect — triggers attachWS, which waits for old pumpDone.
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c2, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("reconnect failed: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	time.Sleep(200 * time.Millisecond)

	// If old writePump didn't exit via pumpQuit, the new pump wouldn't start,
	// and this Send would time out.
	_ = srv.Send("test-connection", wspulse.Frame{Event: "post-resume"})
	_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, msg, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage on resumed connection failed: %v", err)
	}
	f, _ := wspulse.JSONCodec.Decode(msg)
	if f.Event != "post-resume" {
		t.Errorf("Event: want %q, got %q", "post-resume", f.Event)
	}
}

// ── No OnMessage callback: readPump should still process frames ──────────────

func TestServer_NoOnMessage_ReadPumpStillProcesses(t *testing.T) {
	srv := wspulse.NewServer(acceptAll) // no OnMessage callback
	t.Cleanup(srv.Close)
	c := dialTestServer(t, srv)

	// Send a frame — readPump should process without crash.
	data, _ := wspulse.JSONCodec.Encode(wspulse.Frame{Event: "ignored"})
	if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
}

// ── handleRegister stateClosed path (app-closed then reconnect) ──────────────

func TestServer_Resume_ConnectionCloseWhileSuspended_ThenReconnect(t *testing.T) {
	// Hits the stateClosed path in handleRegister: session is closed by
	// the application while suspended, then a reconnect arrives before
	// the grace timer fires.
	connected := make(chan struct{}, 4)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(10), // long window so grace won't expire
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	c1, ts := dialTestServerRaw(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first connect")
	}

	// Close transport → session suspends.
	_ = c1.Close()
	time.Sleep(200 * time.Millisecond)

	// Get the connection reference and call Close() on it → state = stateClosed
	// but still registered in hub maps (grace timer hasn't fired).
	connections := srv.GetConnections("test-room")
	if len(connections) != 1 {
		t.Fatalf("want 1 connection, got %d", len(connections))
	}
	_ = connections[0].Close()
	time.Sleep(50 * time.Millisecond)

	// Reconnect with the same connectionID → handleRegister sees stateClosed,
	// cleans up stale entry, creates new session.
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c2, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("reconnect failed: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })

	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for new session after stale cleanup")
	}

	// Verify new session works.
	if err := srv.Send("test-connection", wspulse.Frame{Event: "after-stale"}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, msg, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	f, _ := wspulse.JSONCodec.Decode(msg)
	if f.Event != "after-stale" {
		t.Errorf("Event: want %q, got %q", "after-stale", f.Event)
	}
}

// ── Resume: stateClosed path in handleRegister fires onDisconnect ─────────────

func TestServer_Resume_StaleClosedSession_OnDisconnectFires(t *testing.T) {
	// Regression test: handleRegister's stateClosed branch must fire onDisconnect.
	// When Close() is called on a suspended session and a new connection with
	// the same ID arrives before the hub processes the resulting
	// graceExpiredMessage, handleRegister finds the session in stateClosed and
	// previously called removeSession directly — silently dropping onDisconnect.
	// The fix: use disconnectSession so onDisconnect always fires.
	//
	// The race window is narrow; we run many cycles so it is triggered across
	// the -count=3 test runs.
	const cycles = 200

	var connects, disconnects atomic.Int32

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(60), // long window; grace won't expire naturally
		wspulse.WithOnConnect(func(_ wspulse.Connection) { connects.Add(1) }),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) { disconnects.Add(1) }),
	)
	t.Cleanup(srv.Close)

	c, ts := dialTestServerRaw(t, srv)
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}

	// Wait for initial session.
	for connects.Load() < 1 {
		time.Sleep(time.Millisecond)
	}

	for i := 0; i < cycles; i++ {
		// Capture expected total before any async onConnect fires for this cycle.
		target := connects.Load() + 1

		// Suspend the session by dropping the transport.
		_ = c.Close()
		// Allow hub to process transportDied → stateSuspended.
		time.Sleep(20 * time.Millisecond)

		// Call Close() on the suspended session: sets stateClosed and fires
		// timer.Reset(0), which enqueues graceExpiredMessage via a goroutine.
		conns := srv.GetConnections("test-room")
		if len(conns) > 0 {
			_ = conns[0].Close()
		}

		// Reconnect immediately — races the graceExpiredMessage goroutine.
		var err error
		c, _, err = dialer.Dial(u, nil)
		if err != nil {
			t.Fatalf("Dial failed on cycle %d: %v", i, err)
		}

		// Wait for new session's onConnect before next cycle.
		deadline := time.NewTimer(3 * time.Second)
		for connects.Load() < target {
			select {
			case <-deadline.C:
				t.Fatalf("timeout waiting for connect on cycle %d", i)
			default:
				time.Sleep(time.Millisecond)
			}
		}
		deadline.Stop()
	}

	// Close last transport; srv.Close() (via t.Cleanup) fires onDisconnect for
	// the remaining session. We call it explicitly here to block until done.
	_ = c.Close()
	srv.Close()

	// Every session that received onConnect must receive onDisconnect.
	// connects = 1 (initial) + cycles (reconnects) = cycles+1.
	// disconnects = cycles (old sessions) + 1 (last session via srv.Close) = cycles+1.
	wantDisconnects := int32(cycles + 1)
	deadline := time.After(5 * time.Second)
	for disconnects.Load() < wantDisconnects {
		select {
		case <-deadline:
			t.Fatalf("connects=%d disconnects=%d: onDisconnect not fired for all sessions",
				connects.Load(), disconnects.Load())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// ── Broadcast covers done-check skip path ────────────────────────────────────

func TestServer_Broadcast_SkipsDirectlyClosedSession(t *testing.T) {
	// When an application calls connection.Close() directly (not via Kick),
	// the session remains in the hub maps but has done closed. A subsequent
	// broadcast should silently skip this session via the <-target.done check.
	connected := make(chan wspulse.Connection, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			connected <- connection
		}),
	)
	t.Cleanup(srv.Close)
	_ = dialTestServer(t, srv)

	var connection wspulse.Connection
	select {
	case connection = <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Close the connection directly — done is closed, but session stays in maps.
	_ = connection.Close()

	// Broadcast should skip the closed session without error.
	err := srv.Broadcast("test-room", wspulse.Frame{Event: "skip-me"})
	if err != nil {
		t.Fatalf("Broadcast returned error: %v", err)
	}
}

// ── writePump: detach with long ping to ensure pumpQuit fires first ──────────

func TestServer_Resume_WritePumpExitsViaPumpQuit(t *testing.T) {
	// With a long ping period, writePump is guaranteed to be idle in the
	// main select when pumpQuit fires (no tick to cause a write error first).
	connected := make(chan struct{}, 2)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5),
		wspulse.WithHeartbeat(5*time.Second, 30*time.Second), // long ping period
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	c1, ts := dialTestServerRaw(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Close client transport to trigger suspend. With long ping period,
	// writePump won't attempt any writes before pumpQuit fires.
	_ = c1.Close()
	time.Sleep(500 * time.Millisecond) // allow hub to process transportDied

	// Reconnect to prove the old writePump has exited correctly.
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c2, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("reconnect failed: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	time.Sleep(200 * time.Millisecond)

	_ = srv.Send("test-connection", wspulse.Frame{Event: "verify"})
	_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, msg, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	f, _ := wspulse.JSONCodec.Decode(msg)
	if f.Event != "verify" {
		t.Errorf("Event: want %q, got %q", "verify", f.Event)
	}
}

// ── Session.Send when done closes concurrently ──────────────────────────────

func TestServer_ConnectionSend_DoneClosesDuringEnqueue(t *testing.T) {
	// Exercises the <-s.done path inside enqueue's three-way select
	// by racing Send calls against Close.
	connected := make(chan wspulse.Connection, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithSendBufferSize(1),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			connected <- connection
		}),
	)
	t.Cleanup(srv.Close)
	_ = dialTestServer(t, srv)

	var connection wspulse.Connection
	select {
	case connection = <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Flood the send buffer then close, racing enqueue against done.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			err := connection.Send(wspulse.Frame{Event: "flood"})
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		_ = connection.Close()
	}()
	wg.Wait()
}

// TestServer_Broadcast_AfterClose verifies Broadcast returns ErrServerClosed
// when called after the server has been closed.
func TestServer_Broadcast_AfterClose(t *testing.T) {
	srv := wspulse.NewServer(acceptAll)
	srv.Close()
	err := srv.Broadcast("test-room", wspulse.Frame{Event: "hello"})
	if !errors.Is(err, wspulse.ErrServerClosed) {
		t.Fatalf("want ErrServerClosed, got %v", err)
	}
}

// TestServer_Kick_AfterClose verifies Kick returns ErrServerClosed
// when called after the server has been closed.
func TestServer_Kick_AfterClose(t *testing.T) {
	srv := wspulse.NewServer(acceptAll)
	srv.Close()
	err := srv.Kick("test-connection")
	if !errors.Is(err, wspulse.ErrServerClosed) {
		t.Fatalf("want ErrServerClosed, got %v", err)
	}
}

// TestServer_ReadPump_NormalCloseFrame covers the readPump path where a client
// sends a proper WebSocket close frame with CloseGoingAway. This triggers
// the else branch of IsUnexpectedCloseError (the "connection closed normally"
// debug log), because CloseGoingAway is in the expected close code list.
func TestServer_ReadPump_NormalCloseFrame(t *testing.T) {
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			disconnected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}

	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Send a proper WebSocket close frame with CloseGoingAway (1001).
	// gorilla returns a *CloseError{Code: 1001}. Since 1001 is in the
	// expected-codes list, IsUnexpectedCloseError returns false, hitting
	// the else branch.
	time.Sleep(100 * time.Millisecond) // ensure readPump is blocked on ReadMessage
	closeMsg := websocket.FormatCloseMessage(websocket.CloseGoingAway, "")
	if err := c.WriteMessage(websocket.CloseMessage, closeMsg); err != nil {
		t.Fatalf("WriteMessage close failed: %v", err)
	}

	// Keep reading to allow gorilla's close handshake to complete.
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, readErr := c.ReadMessage()
		if readErr != nil {
			break
		}
	}

	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for onDisconnect")
	}

	// Close the client connection after the server has finished cleanup.
	_ = c.Close()
}

// TestServer_ConnectionClose_StateClosed_TransportDied covers the
// handleTransportDied path where the session is already in stateClosed
// (set by Connection.Close()) when the transport-died event arrives.
// The hub should still clean up maps and fire onDisconnect.
func TestServer_ConnectionClose_StateClosed_TransportDied(t *testing.T) {
	var capturedConn wspulse.Connection
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			capturedConn = c
			connected <- struct{}{}
		}),
		wspulse.WithOnDisconnect(func(c wspulse.Connection, err error) {
			disconnected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	_ = dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Close the session externally — this sets state to stateClosed
	// and closes done. writePump sees done, sends close frame, closes
	// transport. readPump gets a read error and sends transportDied.
	// When the hub processes transportDied, state is already stateClosed.
	_ = capturedConn.Close()

	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for onDisconnect after Connection.Close()")
	}
}

// TestServer_Resume_GraceTimerFiresAfterReconnect covers the
// handleGraceExpired path where the session has already been
// successfully resumed (stateConnected) by the time the grace timer
// fires. The hub should skip destruction and the session stays alive.
func TestServer_Resume_GraceTimerFiresAfterReconnect(t *testing.T) {
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(1), // minimum 1s
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	// First connection.
	c1, ts := dialTestServerRaw(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first connect")
	}

	// Disconnect (triggers suspend + 1s grace timer).
	_ = c1.Close()
	time.Sleep(200 * time.Millisecond)

	// Reconnect before the grace timer fires. Resume does not fire
	// onConnect — it only calls attachWS, so we verify via Send instead.
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c2, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("reconnect dial failed: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	time.Sleep(200 * time.Millisecond)

	// Wait for the grace timer to fire (1s window + margin).
	// The timer should find the session in stateConnected and skip.
	time.Sleep(1500 * time.Millisecond)

	// Session should still be alive — verify by sending a frame.
	frame := wspulse.Frame{Event: "still-alive", Payload: []byte(`"ok"`)}
	if err := srv.Send("test-connection", frame); err != nil {
		t.Fatalf("Send after grace timer expired should succeed: %v", err)
	}
	_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	f, _ := wspulse.JSONCodec.Decode(message)
	if f.Event != "still-alive" {
		t.Errorf("want type %q, got %q", "still-alive", f.Event)
	}

	select {
	case <-disconnected:
		t.Fatal("onDisconnect should not fire — session was resumed before timer")
	default:
	}
}

// TestServer_Resume_StaleGraceTimer covers the handleGraceExpired path
// where the timer's epoch doesn't match the session's current suspendEpoch.
// This happens when a session is suspended, resumed, and suspended again —
// the first timer fires with the old epoch and should be ignored.
func TestServer_Resume_StaleGraceTimer(t *testing.T) {
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(1), // minimum 1s
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	// First connection.
	c1, ts := dialTestServerRaw(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first connect")
	}

	// Disconnect (suspend epoch=1, timer A set for 1s).
	_ = c1.Close()
	time.Sleep(200 * time.Millisecond)

	// Reconnect quickly. Resume doesn't fire onConnect.
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c2, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("reconnect dial failed: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Disconnect again (suspend epoch=2, timer B set for 1s).
	_ = c2.Close()
	time.Sleep(200 * time.Millisecond)

	// Timer A fires at ~1s with epoch=1. Current epoch is 2 → stale, ignored.
	// Timer B fires at ~1.4s with epoch=2 → matches → session destroyed.
	select {
	case <-disconnected:
		// Only one onDisconnect should fire (from timer B).
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for onDisconnect from second grace timer")
	}
}

// TestServer_ConnectionClose_StateClosed_Resume covers handleTransportDied when
// resume is enabled and connection.Close() was called externally. The session
// is in stateClosed before the transport dies. The hub enters the stateClosed
// case of the non-connected switch and cleans up.
func TestServer_ConnectionClose_StateClosed_Resume(t *testing.T) {
	var capturedConn wspulse.Connection
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5),
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			capturedConn = c
			connected <- struct{}{}
		}),
		wspulse.WithOnDisconnect(func(c wspulse.Connection, err error) {
			disconnected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	_ = dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Close externally → state becomes stateClosed before transport dies.
	// With resume enabled, the hub still enters the state != stateConnected
	// switch and hits the stateClosed case.
	_ = capturedConn.Close()

	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for onDisconnect")
	}
}

// TestServer_Resume_DrainBufferFull covers the attachWS drain path where the
// send buffer is full during resume drain. With a send buffer of size 1 and
// multiple buffered frames, the drain loop must apply drop-oldest backpressure
// to enqueue resume frames.
func TestServer_Resume_DrainBufferFull(t *testing.T) {
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithSendBufferSize(1),
		wspulse.WithResumeWindow(5),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	c1, ts := dialTestServerRaw(t, srv)
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Disconnect to enter suspended state.
	_ = c1.Close()
	time.Sleep(200 * time.Millisecond)

	// Send multiple frames while suspended — they go to the resume buffer.
	// With send buffer size 1, draining 3 frames overflows the send channel
	// and triggers the drop-oldest backpressure in the drain loop.
	for i := 0; i < 3; i++ {
		_ = srv.Send("test-connection", wspulse.Frame{
			Event:   "buffered",
			Payload: []byte(`"` + string(rune('a'+i)) + `"`),
		})
	}

	// Reconnect — the transition goroutine drains the resume buffer into
	// the send channel, hitting the "buffer full" path.
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c2, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("reconnect dial failed: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })

	// Read at least one frame to verify the session is functional.
	_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	f, _ := wspulse.JSONCodec.Decode(message)
	if f.Event != "buffered" {
		t.Errorf("want type %q, got %q", "buffered", f.Event)
	}

	// Session should still be alive.
	select {
	case <-disconnected:
		t.Fatal("onDisconnect should not fire after successful resume")
	default:
	}
}

// TestServer_Resume_ConnectionCloseWhileSuspended_FiresOnDisconnect verifies
// that calling Connection.Close() on a suspended session eventually fires
// onDisconnect once the grace timer expires. This covers the stateClosed
// path in handleGraceExpired.
func TestServer_Resume_ConnectionCloseWhileSuspended_FiresOnDisconnect(t *testing.T) {
	connected := make(chan wspulse.Connection, 1)
	disconnected := make(chan struct{})
	var once sync.Once

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(1), // 1-second grace window
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- connection:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			once.Do(func() { close(disconnected) })
		}),
	)
	t.Cleanup(srv.Close)

	c := dialTestServer(t, srv)
	var connection wspulse.Connection
	select {
	case connection = <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Drop the transport — session enters suspended state.
	_ = c.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "dropping"))
	_ = c.Close()
	time.Sleep(200 * time.Millisecond) // let hub process transportDied

	// Confirm the session is still registered (suspended, not yet cleaned up).
	if conns := srv.GetConnections("test-room"); len(conns) == 0 {
		t.Fatal("session was removed before grace window elapsed; expected it to be suspended")
	}

	// Application calls Close() on the suspended Connection.
	_ = connection.Close()

	// onDisconnect must fire after Connection.Close() on a suspended session.
	// The exact timing is verified by TestServer_Resume_ConnectionClose_ImmediateOnDisconnect.
	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("onDisconnect did not fire after Connection.Close() on suspended session")
	}

	// Session must be removed from hub maps after onDisconnect.
	time.Sleep(50 * time.Millisecond)
	if conns := srv.GetConnections("test-room"); len(conns) != 0 {
		t.Errorf("want 0 connections after Close + grace expiry, got %d", len(conns))
	}
}

// TestServer_Resume_ConnectionClose_ImmediateOnDisconnect verifies that calling
// Connection.Close() on a suspended session fires onDisconnect immediately
// (within a short window), not after the full grace timer expires.
//
// Regression: previously onDisconnect was delayed until the grace timer
// fired naturally, even if the application had already called Close().
func TestServer_Resume_ConnectionClose_ImmediateOnDisconnect(t *testing.T) {
	connected := make(chan wspulse.Connection, 1)
	disconnected := make(chan struct{})
	var once sync.Once

	const gracePeriod = 5 * time.Second // long enough to expose the bug

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(int(gracePeriod.Seconds())),
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			select {
			case connected <- connection:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			once.Do(func() { close(disconnected) })
		}),
	)
	t.Cleanup(srv.Close)

	c := dialTestServer(t, srv)
	var connection wspulse.Connection
	select {
	case connection = <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Drop the transport — session enters suspended state.
	_ = c.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "dropping"))
	_ = c.Close()
	time.Sleep(200 * time.Millisecond) // let hub process transportDied

	// Application calls Close() on the suspended session.
	start := time.Now()
	_ = connection.Close()

	// onDisconnect must fire promptly after Close(), not after the full 5-second
	// grace window. Allow up to 500ms for hub round-trip overhead.
	const wantWithin = 500 * time.Millisecond
	select {
	case <-disconnected:
		if elapsed := time.Since(start); elapsed > wantWithin {
			t.Fatalf("onDisconnect took %v after Connection.Close(), want < %v", elapsed, wantWithin)
		}
	case <-time.After(gracePeriod):
		t.Fatalf("onDisconnect did not fire within %v; fired after full grace window instead", gracePeriod)
	}

	// Session must be removed from hub maps after onDisconnect.
	time.Sleep(50 * time.Millisecond)
	if conns := srv.GetConnections("test-room"); len(conns) != 0 {
		t.Errorf("want 0 connections after Close, got %d", len(conns))
	}
}

// TestServer_Resume_MassCloseWhileSuspended_AllOnDisconnect verifies that
// when many suspended sessions call Close() concurrently, every single
// onDisconnect callback fires. This is a regression test for a bug where,
// under heavy concurrent Close() calls on suspended sessions, some
// onDisconnect callbacks were never invoked due to the internal close
// coordination logic dropping events.
func TestServer_Resume_MassCloseWhileSuspended_AllOnDisconnect(t *testing.T) {
	const count = 200

	var mu sync.Mutex
	connections := make([]wspulse.Connection, 0, count)
	allConnected := make(chan struct{})
	disconnectCount := atomic.Int64{}
	allDisconnected := make(chan struct{})

	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "room", "", nil // auto UUID per connection
		},
		wspulse.WithResumeWindow(30), // long grace window
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			mu.Lock()
			connections = append(connections, connection)
			n := len(connections)
			mu.Unlock()
			if n == count {
				close(allConnected)
			}
		}),
		wspulse.WithOnDisconnect(func(connection wspulse.Connection, err error) {
			if disconnectCount.Add(1) == int64(count) {
				close(allDisconnected)
			}
		}),
	)
	t.Cleanup(srv.Close)

	// Dial count connections.
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	websockets := make([]*websocket.Conn, count)
	for i := 0; i < count; i++ {
		c, _, err := websocket.DefaultDialer.Dial(u, nil)
		if err != nil {
			t.Fatalf("Dial %d: %v", i, err)
		}
		websockets[i] = c
	}

	select {
	case <-allConnected:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for all connections")
	}

	// Drop all transports → all sessions enter suspended state.
	for _, ws := range websockets {
		_ = ws.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = ws.Close()
	}
	time.Sleep(1 * time.Second) // let hub process all transportDied messages

	// Close all suspended sessions concurrently to saturate closeRequests buffer.
	mu.Lock()
	snapshot := make([]wspulse.Connection, len(connections))
	copy(snapshot, connections)
	mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range snapshot {
		wg.Add(1)
		go func(conn wspulse.Connection) {
			defer wg.Done()
			_ = conn.Close()
		}(c)
	}
	wg.Wait()

	select {
	case <-allDisconnected:
	case <-time.After(5 * time.Second):
		got := disconnectCount.Load()
		t.Fatalf("want %d onDisconnect calls, got %d (lost %d)", count, got, count-got)
	}
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestServer_Resume_CloseRacesTransportDied verifies that onDisconnect fires
// promptly even when Close() races with handleTransportDied's grace timer
// setup. There is a window between detachWS (state → stateSuspended) and
// graceTimer assignment where Close() sees graceTimer == nil and cannot call
// timer.Reset(0). Without a fix, onDisconnect is delayed until the full
// resume window expires.
//
// To maximise the chance of hitting this window, the test closes the
// transport and immediately calls Close() without waiting for suspension.
func TestServer_Resume_CloseRacesTransportDied(t *testing.T) {
	const count = 50

	connected := make(chan wspulse.Connection, count)
	disconnectCount := atomic.Int64{}
	allDisconnected := make(chan struct{})

	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "room", "", nil
		},
		wspulse.WithResumeWindow(30), // long window — if delayed, test times out
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			connected <- c
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			if disconnectCount.Add(1) == int64(count) {
				close(allDisconnected)
			}
		}),
	)
	t.Cleanup(srv.Close)

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	type pair struct {
		ws   *websocket.Conn
		conn wspulse.Connection
	}
	pairs := make([]pair, count)
	for i := 0; i < count; i++ {
		ws, _, err := websocket.DefaultDialer.Dial(u, nil)
		if err != nil {
			t.Fatalf("Dial %d: %v", i, err)
		}
		c := <-connected
		pairs[i] = pair{ws: ws, conn: c}
	}

	// For each pair: close the transport and call Close() back-to-back
	// with NO sleep in between, so Close() may execute before the hub
	// has finished assigning graceTimer in handleTransportDied.
	var wg sync.WaitGroup
	for _, p := range pairs {
		wg.Add(1)
		go func(p pair) {
			defer wg.Done()
			_ = p.ws.Close()
			_ = p.conn.Close()
		}(p)
	}
	wg.Wait()

	select {
	case <-allDisconnected:
	case <-time.After(3 * time.Second):
		got := disconnectCount.Load()
		t.Fatalf("want %d onDisconnect within 3s, got %d (delayed by grace window?)", count, got)
	}
}
