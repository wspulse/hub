//go:build integration

package wspulse_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	wspulse "github.com/wspulse/server"
)

func (r *recordingCollector) snapshot() []metricsEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]metricsEvent, len(r.events))
	copy(out, r.events)
	return out
}

func (r *recordingCollector) countByName(name string) int {
	events := r.snapshot()
	n := 0
	for _, e := range events {
		if e.name == name {
			n++
		}
	}
	return n
}

func (r *recordingCollector) eventsByName(name string) []metricsEvent {
	events := r.snapshot()
	var out []metricsEvent
	for _, e := range events {
		if e.name == name {
			out = append(out, e)
		}
	}
	return out
}

func TestIntegration_MetricsCollector_ConnectionLifecycle(t *testing.T) {
	rec := &recordingCollector{}
	connected := make(chan struct{}, 4)
	disconnected := make(chan struct{}, 4)

	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "metrics-room", "", nil
		},
		wspulse.WithMetrics(rec),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			disconnected <- struct{}{}
		}),
	)
	ts := httptest.NewServer(srv)
	defer func() {
		srv.Close()
		ts.Close()
	}()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}

	// Open connection.
	c, resp, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	if n := rec.countByName("RoomCreated"); n != 1 {
		t.Errorf("RoomCreated: want 1, got %d", n)
	}
	if n := rec.countByName("ConnectionOpened"); n != 1 {
		t.Errorf("ConnectionOpened: want 1, got %d", n)
	}

	// Close connection.
	_ = c.Close()
	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for disconnect")
	}

	if n := rec.countByName("ConnectionClosed"); n != 1 {
		t.Errorf("ConnectionClosed: want 1, got %d", n)
	}
	if n := rec.countByName("RoomDestroyed"); n != 1 {
		t.Errorf("RoomDestroyed: want 1, got %d", n)
	}

	// Verify ConnectionClosed has non-negative duration and normal reason.
	for _, e := range rec.eventsByName("ConnectionClosed") {
		if e.duration < 0 {
			t.Errorf("ConnectionClosed duration should be >= 0, got %v", e.duration)
		}
		if e.reason != wspulse.DisconnectNormal {
			t.Errorf("ConnectionClosed reason = %q, want %q", e.reason, wspulse.DisconnectNormal)
		}
	}
}

func TestIntegration_MetricsCollector_MessageFlow(t *testing.T) {
	rec := &recordingCollector{}
	connected := make(chan struct{}, 4)
	broadcastDone := make(chan struct{}, 1)

	var srv wspulse.Server
	srv = wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "metrics-room", "", nil
		},
		wspulse.WithMetrics(rec),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnMessage(func(conn wspulse.Connection, f wspulse.Frame) {
			_ = srv.Broadcast(conn.RoomID(), f)
			select {
			case broadcastDone <- struct{}{}:
			default:
			}
		}),
	)
	ts := httptest.NewServer(srv)
	defer func() {
		srv.Close()
		ts.Close()
	}()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}

	// Open 2 connections.
	c1, resp1, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial c1: %v", err)
	}
	if resp1 != nil && resp1.Body != nil {
		resp1.Body.Close()
	}
	c2, resp2, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial c2: %v", err)
	}
	if resp2 != nil && resp2.Body != nil {
		resp2.Body.Close()
	}
	defer c1.Close()
	defer c2.Close()
	for i := 0; i < 2; i++ {
		select {
		case <-connected:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for connection %d", i+1)
		}
	}

	// Send a message from c1.
	if err := c1.WriteMessage(websocket.TextMessage, []byte(`{"event":"test"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Wait for broadcast to complete.
	select {
	case <-broadcastDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for broadcast")
	}

	// Read broadcast on both clients to ensure writePump sent them (MessageSent fires).
	c1.SetReadDeadline(time.Now().Add(3 * time.Second))
	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = c1.ReadMessage()
	_, _, _ = c2.ReadMessage()

	// Poll until all expected metrics are recorded. SendBufferUtilization
	// fires in the writePump after each write — on CI under the race
	// detector the second writePump may not have recorded the event by the
	// time ReadMessage returns on the client side.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if rec.countByName("MessageSent") >= 2 && rec.countByName("SendBufferUtilization") >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: MessageSent=%d (want 2), SendBufferUtilization=%d (want 2)",
				rec.countByName("MessageSent"), rec.countByName("SendBufferUtilization"))
		}
		time.Sleep(5 * time.Millisecond)
	}

	if n := rec.countByName("MessageReceived"); n != 1 {
		t.Errorf("MessageReceived: want 1, got %d", n)
	}
	if n := rec.countByName("MessageBroadcast"); n != 1 {
		t.Errorf("MessageBroadcast: want 1, got %d", n)
	}
}

func TestIntegration_MetricsCollector_ResumeAttempt(t *testing.T) {
	rec := &recordingCollector{}
	connected := make(chan struct{}, 4)
	transportDrop := make(chan struct{}, 4)
	transportRestore := make(chan struct{}, 4)

	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "resume-room", "resume-conn", nil
		},
		wspulse.WithMetrics(rec),
		wspulse.WithResumeWindow(5*time.Second),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnTransportDrop(func(_ wspulse.Connection, _ error) {
			transportDrop <- struct{}{}
		}),
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			transportRestore <- struct{}{}
		}),
	)
	ts := httptest.NewServer(srv)
	defer func() {
		srv.Close()
		ts.Close()
	}()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}

	// Initial connection.
	c1, resp1, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if resp1 != nil && resp1.Body != nil {
		resp1.Body.Close()
	}
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	if n := rec.countByName("ConnectionOpened"); n != 1 {
		t.Errorf("ConnectionOpened: want 1, got %d", n)
	}

	// Drop transport.
	_ = c1.Close()
	select {
	case <-transportDrop:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for transport drop")
	}

	// Reconnect with same ID → resume.
	c2, resp2, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Reconnect Dial: %v", err)
	}
	if resp2 != nil && resp2.Body != nil {
		resp2.Body.Close()
	}
	defer c2.Close()

	select {
	case <-transportRestore:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for transport restore")
	}

	if n := rec.countByName("ResumeAttempt"); n != 1 {
		t.Errorf("ResumeAttempt: want 1, got %d", n)
	}
	// Should NOT fire a second ConnectionOpened (session was resumed, not recreated).
	if n := rec.countByName("ConnectionOpened"); n != 1 {
		t.Errorf("ConnectionOpened after resume: want 1, got %d", n)
	}

	events := rec.eventsByName("ResumeAttempt")
	if len(events) > 0 && !events[0].success {
		t.Errorf("ResumeAttempt success = false, want true")
	}
}

func TestIntegration_MetricsCollector_FrameDropped_SendFull(t *testing.T) {
	rec := &recordingCollector{}
	connected := make(chan wspulse.Connection, 1)

	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "drop-room", "drop-conn", nil
		},
		wspulse.WithMetrics(rec),
		wspulse.WithSendBufferSize(1),
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			connected <- c
		}),
	)
	ts := httptest.NewServer(srv)
	defer func() {
		srv.Close()
		ts.Close()
	}()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c, resp, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	defer c.Close()

	var conn wspulse.Connection
	select {
	case conn = <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	// Rapid-fire send to trigger at least one buffer-full drop.
	// With send buffer size 1, the writePump may drain between individual
	// sends, so we loop until at least one ErrSendBufferFull is observed.
	frame := wspulse.Frame{Event: "fill", Payload: []byte(`{}`)}
	var gotBufferFull bool
	for i := 0; i < 200; i++ {
		if sendErr := conn.Send(frame); sendErr == wspulse.ErrSendBufferFull {
			gotBufferFull = true
			break
		}
	}
	if !gotBufferFull {
		t.Fatal("expected at least one ErrSendBufferFull in 200 rapid sends")
	}

	if n := rec.countByName("FrameDropped"); n < 1 {
		t.Errorf("FrameDropped: want >= 1, got %d", n)
	}
}

func TestIntegration_MetricsCollector_FrameDropped_BroadcastDropOldest(t *testing.T) {
	rec := &recordingCollector{}
	connected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "drop-room", "drop-conn", nil
		},
		wspulse.WithMetrics(rec),
		wspulse.WithSendBufferSize(1),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	ts := httptest.NewServer(srv)
	defer func() {
		srv.Close()
		ts.Close()
	}()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c, resp, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	defer c.Close()

	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	frame := wspulse.Frame{Event: "fill", Payload: []byte(`{}`)}
	// Fill buffer via broadcast (drop-oldest path).
	_ = srv.Broadcast("drop-room", frame)
	// Second broadcast triggers drop-oldest on the full buffer.
	_ = srv.Broadcast("drop-room", frame)
	// Third broadcast to ensure at least one drop-oldest fires.
	_ = srv.Broadcast("drop-room", frame)

	// Poll until FrameDropped is observed instead of sleeping.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if n := rec.countByName("FrameDropped"); n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for FrameDropped, got %d", rec.countByName("FrameDropped"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestIntegration_MetricsCollector_PongTimeout(t *testing.T) {
	rec := &recordingCollector{}
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "timeout-room", "timeout-conn", nil
		},
		wspulse.WithMetrics(rec),
		// Very short pongWait so the test doesn't take long.
		wspulse.WithHeartbeat(50*time.Millisecond, 100*time.Millisecond),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	ts := httptest.NewServer(srv)
	defer func() {
		srv.Close()
		ts.Close()
	}()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	// Use a raw dialer that does NOT respond to pings.
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c, resp, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}

	// Disable the default pong handler so the server's pongWait expires.
	c.SetPingHandler(func(string) error { return nil })
	// Must read to process control frames; the read will eventually fail.
	go func() {
		for {
			_, _, err := c.ReadMessage()
			if err != nil {
				return
			}
		}
	}()

	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for pong timeout disconnect")
	}
	_ = c.Close()

	if n := rec.countByName("PongTimeout"); n != 1 {
		t.Errorf("PongTimeout: want 1, got %d", n)
	}
}

func TestIntegration_MetricsCollector_Shutdown(t *testing.T) {
	rec := &recordingCollector{}
	connected := make(chan struct{}, 4)

	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "shutdown-room", "", nil
		},
		wspulse.WithMetrics(rec),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
	)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}

	// Open 2 connections.
	c1, resp1, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial c1: %v", err)
	}
	if resp1 != nil && resp1.Body != nil {
		resp1.Body.Close()
	}
	c2, resp2, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial c2: %v", err)
	}
	if resp2 != nil && resp2.Body != nil {
		resp2.Body.Close()
	}
	defer c1.Close()
	defer c2.Close()
	for i := 0; i < 2; i++ {
		select {
		case <-connected:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for connection %d", i+1)
		}
	}

	// Shutdown the server.
	srv.Close()

	// Verify ConnectionClosed fired for both connections with server_close reason.
	closedEvents := rec.eventsByName("ConnectionClosed")
	if len(closedEvents) != 2 {
		t.Fatalf("ConnectionClosed: want 2, got %d", len(closedEvents))
	}
	for _, e := range closedEvents {
		if e.reason != wspulse.DisconnectServerClose {
			t.Errorf("ConnectionClosed reason = %q, want %q", e.reason, wspulse.DisconnectServerClose)
		}
	}

	// Verify RoomDestroyed fired.
	if n := rec.countByName("RoomDestroyed"); n != 1 {
		t.Errorf("RoomDestroyed: want 1, got %d", n)
	}
}

// TestIntegration_MetricsCollector_ResumeAttempt_Rejected verifies that when a
// client reconnects with ?resume=true after the grace window has expired, the
// server returns HTTP 410 Gone and emits ResumeAttempt(roomID, connectionID, false).
// No ConnectionOpened should fire for the rejected reconnect attempt.
func TestIntegration_MetricsCollector_ResumeAttempt_Rejected(t *testing.T) {
	rec := &recordingCollector{}
	connected := make(chan struct{}, 2)
	disconnected := make(chan struct{}, 2)
	dropped := make(chan struct{}, 2)
	fc := newFakeClock()

	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "resume-room", "resume-conn", nil
		},
		wspulse.WithMetrics(rec),
		wspulse.WithResumeWindow(3*time.Minute),
		wspulse.WithClock(fc),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnTransportDrop(func(_ wspulse.Connection, _ error) {
			select {
			case dropped <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	ts := httptest.NewServer(srv)
	defer func() {
		srv.Close()
		ts.Close()
	}()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}

	// Step 1: Initial connection.
	c1, resp1, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if resp1 != nil && resp1.Body != nil {
		resp1.Body.Close()
	}
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connect")
	}

	if n := rec.countByName("ConnectionOpened"); n != 1 {
		t.Fatalf("ConnectionOpened after initial connect: want 1, got %d", n)
	}

	// Step 2: Drop transport → session suspends.
	_ = c1.Close()
	select {
	case <-dropped:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for transport drop")
	}

	// Step 3: Fire grace timer → session destroyed, onDisconnect fires.
	fc.Fire(0)
	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for disconnect after grace expiry")
	}

	// Step 4: Reconnect with ?resume=true → expect HTTP 410 Gone.
	resumeURL := wsURL + "?resume=true"
	c2, resp2, err := dialer.Dial(resumeURL, nil)
	if c2 != nil {
		c2.Close()
	}

	// The server should reject with HTTP 410 — the dial must fail.
	if err == nil {
		t.Fatal("expected Dial to fail with HTTP 410, but it succeeded")
	}
	if resp2 == nil {
		t.Fatalf("expected HTTP response, got nil (err=%v)", err)
	}
	if resp2.Body != nil {
		io.Copy(io.Discard, resp2.Body)
		resp2.Body.Close()
	}
	if resp2.StatusCode != http.StatusGone {
		t.Errorf("response status: want %d (Gone), got %d", http.StatusGone, resp2.StatusCode)
	}

	// Step 5: Verify ResumeAttempt(false) was emitted.
	deadline := time.Now().Add(3 * time.Second)
	for {
		events := rec.eventsByName("ResumeAttempt")
		for _, e := range events {
			if !e.success && e.roomID == "resume-room" && e.connectionID == "resume-conn" {
				goto resumeFound
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("ResumeAttempt(false) not emitted; events: %v", rec.snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
resumeFound:

	// Step 6: Verify ConnectionOpened was NOT emitted for the rejected reconnect.
	// Only the initial connection should have triggered ConnectionOpened.
	if n := rec.countByName("ConnectionOpened"); n != 1 {
		t.Errorf("ConnectionOpened: want 1 (initial only), got %d", n)
	}
}
