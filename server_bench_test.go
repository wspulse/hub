package wspulse_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	wspulse "github.com/wspulse/hub"
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
func dialN(b *testing.B, srv wspulse.Hub, n int) (*httptest.Server, []*websocket.Conn) {
	b.Helper()
	ts := httptest.NewServer(srv)
	u := "ws" + strings.TrimPrefix(ts.URL, "http")

	conns := make([]*websocket.Conn, n)
	for i := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		c, _, err := websocket.Dial(ctx, u, nil)
		cancel()
		if err != nil {
			b.Fatalf("Dial %d failed: %v", i, err)
		}
		conns[i] = c
	}
	return ts, conns
}

// jsonPayload returns a valid JSON string payload with total byte length size.
// size includes the surrounding quotes; minimum is 2 (an empty JSON string).
func jsonPayload(size int) []byte {
	if size < 2 {
		size = 2
	}
	return []byte(`"` + strings.Repeat("x", size-2) + `"`)
}

// messageSizes is the standard payload size matrix shared by Broadcast and Send
// benchmarks. Values match the workspace bench-harness plan.
var messageSizes = []struct {
	label string
	size  int
}{
	{"64B", 64},
	{"1KiB", 1024},
	{"16KiB", 16 * 1024},
}

func BenchmarkBroadcast(b *testing.B) {
	for _, roomSize := range []int{1, 10, 100, 1000} {
		for _, ms := range messageSizes {
			name := fmt.Sprintf("room=%d/messageSize=%s", roomSize, ms.label)
			b.Run(name, func(b *testing.B) {
				benchBroadcast(b, roomSize, ms.size)
			})
		}
	}
}

func benchBroadcast(b *testing.B, roomSize, payloadSize int) {
	connected := make(chan struct{}, roomSize)
	srv := wspulse.NewHub(
		benchAcceptN(),
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)

	ts, conns := dialN(b, srv, roomSize)
	b.Cleanup(ts.Close)
	b.Cleanup(srv.Close)
	for _, c := range conns {
		conn := c
		b.Cleanup(func() { _ = conn.CloseNow() })
	}

	// Wait for all connections to register.
	for i := 0; i < roomSize; i++ {
		select {
		case <-connected:
		case <-time.After(5 * time.Second):
			b.Fatalf("timed out waiting for connection %d", i)
		}
	}

	msg := wspulse.Message{Event: "bench", Payload: jsonPayload(payloadSize)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := srv.Broadcast("bench-room", msg); err != nil {
			b.Fatalf("Broadcast: %v", err)
		}
	}
}

func BenchmarkSend(b *testing.B) {
	for _, ms := range messageSizes {
		b.Run(fmt.Sprintf("messageSize=%s", ms.label), func(b *testing.B) {
			benchSend(b, ms.size)
		})
	}
}

func benchSend(b *testing.B, payloadSize int) {
	var conn wspulse.Connection
	connected := make(chan struct{}, 1)
	srv := wspulse.NewHub(
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
		wspulse.WithSendBufferSize(4096), // larger buffer to reduce send-buffer-full frequency
	)

	ts := httptest.NewServer(srv)
	b.Cleanup(ts.Close)
	b.Cleanup(srv.Close)

	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	c, _, err := websocket.Dial(ctx, u, nil)
	cancel()
	if err != nil {
		b.Fatalf("Dial failed: %v", err)
	}
	b.Cleanup(func() { _ = c.CloseNow() })

	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		b.Fatal("timed out waiting for connect")
	}

	// Drain client side to prevent send buffer full.
	go func() {
		for {
			_, _, err := c.Read(context.Background())
			if err != nil {
				return
			}
		}
	}()

	msg := wspulse.Message{Event: "bench", Payload: jsonPayload(payloadSize)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// ErrSendBufferFull is expected at benchmark speed — the drain
		// goroutine cannot keep up with the enqueue rate. The benchmark
		// measures raw encode+enqueue cost including both paths.
		_ = conn.Send(msg)
	}
}

func BenchmarkEnqueue_DropOldest(b *testing.B) {
	var conn wspulse.Connection
	connected := make(chan struct{}, 1)
	srv := wspulse.NewHub(
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
		wspulse.WithSendBufferSize(1),
	)

	ts := httptest.NewServer(srv)
	b.Cleanup(ts.Close)
	b.Cleanup(srv.Close)

	u := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	c, _, err := websocket.Dial(ctx, u, nil)
	cancel()
	if err != nil {
		b.Fatalf("Dial failed: %v", err)
	}
	b.Cleanup(func() { _ = c.CloseNow() })

	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		b.Fatal("timed out waiting for connect")
	}

	// Do NOT drain — let buffer fill so broadcast hits drop-oldest path.
	// Pre-fill the send buffer via direct Send (bypasses hub, synchronous
	// enqueue) to avoid a racy sleep.
	msg := wspulse.Message{Event: "bench", Payload: []byte(`{"v":1}`)}
	if err := conn.Send(msg); err != nil {
		b.Fatalf("prefill Send failed: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := srv.Broadcast("bench-room", msg); err != nil {
			b.Fatalf("Broadcast failed: %v", err)
		}
	}
}

// BenchmarkResumeBufferDrain measures the cost of one suspend → fill resume
// buffer → reconnect cycle. The drain itself runs inside the heart goroutine
// during attachWS, before onTransportRestore fires. The bench uses mock
// transports (via InjectTransport) to avoid WebSocket dial overhead, so the
// measurement is dominated by buffer drain + heart scheduling rather than
// network noise.
//
// The fill phase (256 broadcasts to a suspended session) is bench-internal
// setup and is excluded from the timed region via b.StopTimer/StartTimer.
func BenchmarkResumeBufferDrain(b *testing.B) {
	const (
		bufferSize   = 256
		connectionID = "bench-conn"
		roomID       = "bench-room"
	)

	connected := make(chan struct{}, 1)
	dropped := make(chan struct{}, b.N+1)
	restored := make(chan struct{}, b.N+1)

	srv := wspulse.NewHub(
		func(r *http.Request) (string, string, error) {
			return roomID, connectionID, nil
		},
		wspulse.WithResumeWindow(30*time.Second),
		wspulse.WithSendBufferSize(bufferSize),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnTransportDrop(func(_ wspulse.Connection, _ error) {
			dropped <- struct{}{}
		}),
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			restored <- struct{}{}
		}),
	)
	b.Cleanup(srv.Close)

	// Initial connect.
	mt := newMockTransport()
	wspulse.InjectTransport(srv, connectionID, roomID, mt)
	<-connected

	msg := wspulse.Message{Event: "bench", Payload: jsonPayload(64)}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// Drop the current transport, transitioning the session to suspended.
		mt.InjectError(errors.New("bench: transport closed"))
		<-dropped

		// Fill the resume buffer to capacity. Broadcasts during suspension
		// land in resumeBuffer (drop-oldest at full).
		for j := 0; j < bufferSize; j++ {
			if err := srv.Broadcast(roomID, msg); err != nil {
				b.Fatalf("Broadcast(fill): %v", err)
			}
		}

		// Reconnect: a fresh mock transport triggers attachWS, which drains
		// resumeBuffer back into the send queue inside the heart goroutine
		// before onTransportRestore fires.
		mt = newMockTransport()
		b.StartTimer()
		wspulse.InjectTransport(srv, connectionID, roomID, mt)
		<-restored
	}
}
