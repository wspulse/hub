//go:build integration

package wspulse_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.NoError(t, err, "Dial")
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		require.Fail(t, "timed out waiting for connect")
	}

	assert.Equal(t, 1, rec.countByName("RoomCreated"), "RoomCreated")
	assert.Equal(t, 1, rec.countByName("ConnectionOpened"), "ConnectionOpened")

	// Close connection.
	_ = c.Close()
	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		require.Fail(t, "timed out waiting for disconnect")
	}

	assert.Equal(t, 1, rec.countByName("ConnectionClosed"), "ConnectionClosed")
	assert.Equal(t, 1, rec.countByName("RoomDestroyed"), "RoomDestroyed")

	// Verify ConnectionClosed has non-negative duration and normal reason.
	for _, e := range rec.eventsByName("ConnectionClosed") {
		assert.GreaterOrEqual(t, e.duration, time.Duration(0), "ConnectionClosed duration should be >= 0")
		assert.Equal(t, wspulse.DisconnectNormal, e.reason, "ConnectionClosed reason")
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
	require.NoError(t, err, "Dial c1")
	if resp1 != nil && resp1.Body != nil {
		resp1.Body.Close()
	}
	c2, resp2, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err, "Dial c2")
	if resp2 != nil && resp2.Body != nil {
		resp2.Body.Close()
	}
	defer c1.Close()
	defer c2.Close()
	for i := 0; i < 2; i++ {
		select {
		case <-connected:
		case <-time.After(3 * time.Second):
			require.Failf(t, "timed out waiting for connection", "connection %d", i+1)
		}
	}

	// Send a message from c1.
	err = c1.WriteMessage(websocket.TextMessage, []byte(`{"event":"test"}`))
	require.NoError(t, err, "write")

	// Wait for broadcast to complete.
	select {
	case <-broadcastDone:
	case <-time.After(3 * time.Second):
		require.Fail(t, "timed out waiting for broadcast")
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
			require.Failf(t, "timed out waiting for metrics",
				"MessageSent=%d (want 2), SendBufferUtilization=%d (want 2)",
				rec.countByName("MessageSent"), rec.countByName("SendBufferUtilization"))
		}
		time.Sleep(5 * time.Millisecond)
	}

	assert.Equal(t, 1, rec.countByName("MessageReceived"), "MessageReceived")
	assert.Equal(t, 1, rec.countByName("MessageBroadcast"), "MessageBroadcast")
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
	require.NoError(t, err, "Dial")
	if resp1 != nil && resp1.Body != nil {
		resp1.Body.Close()
	}
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		require.Fail(t, "timed out waiting for connect")
	}

	assert.Equal(t, 1, rec.countByName("ConnectionOpened"), "ConnectionOpened")

	// Drop transport.
	_ = c1.Close()
	select {
	case <-transportDrop:
	case <-time.After(3 * time.Second):
		require.Fail(t, "timed out waiting for transport drop")
	}

	// Reconnect with same ID → resume.
	c2, resp2, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err, "Reconnect Dial")
	if resp2 != nil && resp2.Body != nil {
		resp2.Body.Close()
	}
	defer c2.Close()

	select {
	case <-transportRestore:
	case <-time.After(3 * time.Second):
		require.Fail(t, "timed out waiting for transport restore")
	}

	assert.Equal(t, 1, rec.countByName("ResumeAttempt"), "ResumeAttempt")
	// Should NOT fire a second ConnectionOpened (session was resumed, not recreated).
	assert.Equal(t, 1, rec.countByName("ConnectionOpened"), "ConnectionOpened after resume")

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
	require.NoError(t, err, "Dial")
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	defer c.Close()

	var conn wspulse.Connection
	select {
	case conn = <-connected:
	case <-time.After(3 * time.Second):
		require.Fail(t, "timed out waiting for connect")
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
	require.True(t, gotBufferFull, "expected at least one ErrSendBufferFull in 200 rapid sends")

	assert.GreaterOrEqual(t, rec.countByName("FrameDropped"), 1, "FrameDropped")
}

func TestIntegration_MetricsCollector_FrameDropped_BroadcastDropOldest(t *testing.T) {
	rec := &recordingCollector{}
	connected := make(chan struct{}, 1)
	transportDropped := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "drop-room", "drop-conn", nil
		},
		wspulse.WithMetrics(rec),
		wspulse.WithSendBufferSize(1),
		wspulse.WithResumeWindow(10*time.Second),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnTransportDrop(func(_ wspulse.Connection, _ error) {
			select {
			case transportDropped <- struct{}{}:
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
	require.NoError(t, err, "Dial")
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	t.Cleanup(func() { _ = c.Close() })

	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		require.Fail(t, "timed out waiting for connect")
	}

	// Close the WebSocket to suspend the session. With ResumeWindow,
	// writePump stops but the session stays alive. Frames go to the
	// ring buffer (size 1) with no drain — deterministic overflow.
	c.Close()

	select {
	case <-transportDropped:
	case <-time.After(3 * time.Second):
		require.Fail(t, "timed out waiting for transport drop")
	}

	frame := wspulse.Frame{Event: "fill", Payload: []byte(`{}`)}
	// Ring buffer size 1: first broadcast fills it, second triggers
	// drop-oldest. No race with writePump — it's stopped.
	_ = srv.Broadcast("drop-room", frame)
	_ = srv.Broadcast("drop-room", frame)

	// Poll until FrameDropped is observed.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if n := rec.countByName("FrameDropped"); n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			require.Failf(t, "timed out waiting for FrameDropped",
				"got %d", rec.countByName("FrameDropped"))
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
	require.NoError(t, err, "Dial")
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
		require.Fail(t, "timed out waiting for pong timeout disconnect")
	}
	_ = c.Close()

	assert.Equal(t, 1, rec.countByName("PongTimeout"), "PongTimeout")
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
	require.NoError(t, err, "Dial c1")
	if resp1 != nil && resp1.Body != nil {
		resp1.Body.Close()
	}
	c2, resp2, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err, "Dial c2")
	if resp2 != nil && resp2.Body != nil {
		resp2.Body.Close()
	}
	defer c1.Close()
	defer c2.Close()
	for i := 0; i < 2; i++ {
		select {
		case <-connected:
		case <-time.After(3 * time.Second):
			require.Failf(t, "timed out waiting for connection", "connection %d", i+1)
		}
	}

	// Shutdown the server.
	srv.Close()

	// Verify ConnectionClosed fired for both connections with server_close reason.
	closedEvents := rec.eventsByName("ConnectionClosed")
	require.Len(t, closedEvents, 2, "ConnectionClosed")
	for _, e := range closedEvents {
		assert.Equal(t, wspulse.DisconnectServerClose, e.reason, "ConnectionClosed reason")
	}

	// Verify RoomDestroyed fired.
	assert.Equal(t, 1, rec.countByName("RoomDestroyed"), "RoomDestroyed")
}
