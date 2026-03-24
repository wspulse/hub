//go:build integration

package wspulse_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	<-connected

	if n := rec.countByName("RoomCreated"); n != 1 {
		t.Errorf("RoomCreated: want 1, got %d", n)
	}
	if n := rec.countByName("ConnectionOpened"); n != 1 {
		t.Errorf("ConnectionOpened: want 1, got %d", n)
	}

	// Close connection.
	_ = c.Close()
	<-disconnected

	if n := rec.countByName("ConnectionClosed"); n != 1 {
		t.Errorf("ConnectionClosed: want 1, got %d", n)
	}
	if n := rec.countByName("RoomDestroyed"); n != 1 {
		t.Errorf("RoomDestroyed: want 1, got %d", n)
	}

	// Verify ConnectionClosed has non-zero duration.
	for _, e := range rec.snapshot() {
		if e.name == "ConnectionClosed" && e.duration <= 0 {
			t.Errorf("ConnectionClosed duration should be > 0, got %v", e.duration)
		}
	}
}

func TestIntegration_MetricsCollector_MessageFlow(t *testing.T) {
	rec := &recordingCollector{}
	connected := make(chan struct{}, 4)
	var broadcastDone sync.WaitGroup

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
			broadcastDone.Add(1)
			_ = srv.Broadcast(conn.RoomID(), f)
			broadcastDone.Done()
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
	<-connected
	<-connected

	// Send a message from c1.
	if err := c1.WriteMessage(websocket.TextMessage, []byte(`{"event":"test"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Wait for broadcast to complete.
	broadcastDone.Wait()

	// Read broadcast on both clients to ensure writePump sent them (MessageSent fires).
	c1.SetReadDeadline(time.Now().Add(3 * time.Second))
	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = c1.ReadMessage()
	_, _, _ = c2.ReadMessage()

	if n := rec.countByName("MessageReceived"); n != 1 {
		t.Errorf("MessageReceived: want 1, got %d", n)
	}
	if n := rec.countByName("MessageBroadcast"); n != 1 {
		t.Errorf("MessageBroadcast: want 1, got %d", n)
	}
	// 2 connections in room → 2 MessageSent.
	if n := rec.countByName("MessageSent"); n != 2 {
		t.Errorf("MessageSent: want 2, got %d", n)
	}
	// SendBufferUtilization fires after each MessageSent.
	if n := rec.countByName("SendBufferUtilization"); n != 2 {
		t.Errorf("SendBufferUtilization: want 2, got %d", n)
	}
}
