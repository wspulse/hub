package server_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/goleak"

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
	err := srv.Send("does-not-exist", wspulse.Frame{Type: "ping"})
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

func TestServer_OnConnect_SendsFrame(t *testing.T) {
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(connection wspulse.Connection) {
			_ = connection.Send(wspulse.Frame{Type: "welcome", Payload: []byte(`"hello"`)})
		}),
	)
	t.Cleanup(srv.Close)
	c := dialTestServer(t, srv)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	f, err := wspulse.JSONCodec.Decode(message)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if f.Type != "welcome" {
		t.Errorf("Type: want %q, got %q", "welcome", f.Type)
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
	encoded, _ := wspulse.JSONCodec.Encode(wspulse.Frame{Type: "msg", Payload: payload})
	if err := c.WriteMessage(websocket.TextMessage, encoded); err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}
	select {
	case f := <-received:
		if f.Type != "msg" {
			t.Errorf("Type: want %q, got %q", "msg", f.Type)
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
	frame := wspulse.Frame{Type: "notice", Payload: []byte(`"hello room"`)}
	if err := srv.Broadcast("test-room", frame); err != nil {
		t.Fatalf("Broadcast failed: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	f, err := wspulse.JSONCodec.Decode(message)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if f.Type != "notice" {
		t.Errorf("Type: want %q, got %q", "notice", f.Type)
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
	frame := wspulse.Frame{Type: "direct", Payload: []byte(`"hi"`)}
	if err := srv.Send("test-connection", frame); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	f, err := wspulse.JSONCodec.Decode(message)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if f.Type != "direct" {
		t.Errorf("Type: want %q, got %q", "direct", f.Type)
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
	frame := wspulse.Frame{Type: "ok", Payload: []byte(`"after-kick"`)}
	if err := srv.Send("test-connection", frame); err != nil {
		t.Fatalf("Send to second connection failed: %v", err)
	}
	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage on second connection failed: %v", err)
	}
	f, err := wspulse.JSONCodec.Decode(message)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if f.Type != "ok" {
		t.Errorf("Type: want %q, got %q", "ok", f.Type)
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
				_ = srv.Broadcast("room", wspulse.Frame{Type: "ping"})
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
		_ = srv.Broadcast("test-room", wspulse.Frame{Type: typ, Payload: []byte(`"x"`)})
	}

	time.Sleep(300 * time.Millisecond)

	var frames []wspulse.Frame
	for {
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
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
		if f.Type == "newest" {
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
		defer c.Close()
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

	data, _ := wspulse.JSONCodec.Encode(wspulse.Frame{Type: "trigger"})
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
	err := srv.Broadcast("test-room", wspulse.Frame{Type: "msg"})
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
		err := connection.Send(wspulse.Frame{Type: "flood"})
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

	if err := srv.Broadcast("room-a", wspulse.Frame{Type: "hello"}); err != nil {
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

	frame := wspulse.Frame{Type: "after-resume", Payload: []byte(`"ok"`)}
	if err := srv.Send("test-connection", frame); err != nil {
		t.Fatalf("Send after resume failed: %v", err)
	}
	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage on resumed connection failed: %v", err)
	}
	f, _ := wspulse.JSONCodec.Decode(message)
	if f.Type != "after-resume" {
		t.Errorf("Type: want %q, got %q", "after-resume", f.Type)
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
			Type:    "buffered",
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
		c2.SetReadDeadline(time.Now().Add(3 * time.Second))
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
		if f.Type != "buffered" {
			t.Errorf("Type: want %q, got %q", "buffered", f.Type)
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

	_ = srv.Broadcast("test-room", wspulse.Frame{Type: "bcast", Payload: []byte(`"suspended"`)})

	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c2, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("reconnect dial failed: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })

	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, message, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	f, _ := wspulse.JSONCodec.Decode(message)
	if f.Type != "bcast" {
		t.Errorf("Type: want %q, got %q", "bcast", f.Type)
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
				_ = srv.Broadcast("test-room", wspulse.Frame{Type: "ping"})
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

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
