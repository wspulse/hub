//go:build integration

package wspulse_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	wspulse "github.com/wspulse/server"
)

// benchAcceptN returns a ConnectFunc that assigns each connection to
// "bench-room" with a unique sequential ID.
func benchAcceptN() wspulse.ConnectFunc {
	var mu sync.Mutex
	var n int
	return func(r *http.Request) (string, string, error) {
		mu.Lock()
		n++
		id := fmt.Sprintf("conn-%d", n)
		mu.Unlock()
		return "bench-room", id, nil
	}
}

// dialN connects n WebSocket clients to srv, returning the server
// and all connections. Caller must close the server and connections.
func dialN(b *testing.B, srv wspulse.Server, n int) (*httptest.Server, []*websocket.Conn) {
	b.Helper()
	ts := httptest.NewServer(srv)
	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}

	conns := make([]*websocket.Conn, n)
	for i := range conns {
		c, _, err := dialer.Dial(u, nil)
		if err != nil {
			b.Fatalf("Dial %d failed: %v", i, err)
		}
		conns[i] = c
	}
	return ts, conns
}

func BenchmarkBroadcast(b *testing.B) {
	for _, roomSize := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("room_%d", roomSize), func(b *testing.B) {
			benchBroadcast(b, roomSize)
		})
	}
}

func benchBroadcast(b *testing.B, roomSize int) {
	connected := make(chan struct{}, roomSize)
	srv := wspulse.NewServer(
		benchAcceptN(),
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)

	ts, conns := dialN(b, srv, roomSize)
	defer ts.Close()
	defer srv.Close()
	for _, c := range conns {
		defer c.Close()
	}

	// Wait for all connections to register.
	for i := 0; i < roomSize; i++ {
		select {
		case <-connected:
		case <-time.After(5 * time.Second):
			b.Fatalf("timed out waiting for connection %d", i)
		}
	}

	frame := wspulse.Frame{Event: "bench", Payload: []byte(`{"v":1}`)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := srv.Broadcast("bench-room", frame); err != nil {
			b.Fatalf("Broadcast: %v", err)
		}
	}
}

func BenchmarkSend(b *testing.B) {
	var conn wspulse.Connection
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "bench-room", "bench-conn", nil
		},
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			conn = c
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)

	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c, _, err := dialer.Dial(u, nil)
	if err != nil {
		b.Fatalf("Dial failed: %v", err)
	}
	defer c.Close()

	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		b.Fatal("timed out waiting for connect")
	}

	// Drain client side to prevent send buffer full.
	go func() {
		for {
			_, _, err := c.ReadMessage()
			if err != nil {
				return
			}
		}
	}()

	frame := wspulse.Frame{Event: "bench", Payload: []byte(`{"v":1}`)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = conn.Send(frame)
	}
}

func BenchmarkEnqueue_DropOldest(b *testing.B) {
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "bench-room", "bench-conn", nil
		},
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithSendBufferSize(1),
	)

	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	c, _, err := dialer.Dial(u, nil)
	if err != nil {
		b.Fatalf("Dial failed: %v", err)
	}
	defer c.Close()

	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		b.Fatal("timed out waiting for connect")
	}

	// Do NOT drain — let buffer fill so broadcast hits drop-oldest path.
	frame := wspulse.Frame{Event: "bench", Payload: []byte(`{"v":1}`)}

	// Pre-fill the buffer.
	_ = srv.Broadcast("bench-room", frame)
	time.Sleep(50 * time.Millisecond) // let hub process

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = srv.Broadcast("bench-room", frame)
	}
}
