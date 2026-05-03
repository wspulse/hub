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
	for b.Loop() {
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
	for b.Loop() {
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
	for b.Loop() {
		if err := srv.Broadcast("bench-room", msg); err != nil {
			b.Fatalf("Broadcast failed: %v", err)
		}
	}
}

// BenchmarkResumeBufferDrain measures the per-cycle cost of staging a full
// 256-slot resume buffer and draining it back into the send queue. The
// drain logic is what attachWS runs in its transition goroutine when a
// suspended session reconnects; this bench isolates that path from
// transition-goroutine scheduling, InjectTransport routing, and codec
// encode cost so regressions land directly on the buffer-copy operations.
//
// Each iteration:
//   - PrefillResumeBuffer: 256 direct ForcePush calls into resumeBuffer.
//   - DrainResumeBuffer: pops every entry via RingBuffer.Drain and
//     re-enqueues each onto the send queue with ForceEnqueue.
//
// Setup (one-time): connect a mock transport, drop it so the session is in
// stateSuspended (which is what makes resumeBuffer the fill target), and
// pre-encode a representative payload so the loop pays zero codec cost.
//
// Reported ns/op covers (256 ForcePush + 256 ForceEnqueue) per cycle —
// divide by 256 to get amortised per-message cost. The original "full
// reconnect cycle" measurement is intentionally not captured here; it
// rolled in goroutine scheduling and channel routing that obscured drain
// regressions.
func BenchmarkResumeBufferDrain(b *testing.B) {
	const (
		bufferSize   = 256
		connectionID = "bench-conn"
		roomID       = "bench-room"
	)

	connected := make(chan struct{}, 1)
	dropped := make(chan struct{}, 1)

	srv := wspulse.NewHub(
		func(r *http.Request) (string, string, error) {
			return roomID, connectionID, nil
		},
		// resumeWindow only needs to outlast the bench setup; the drain
		// hooks bypass the grace timer entirely.
		wspulse.WithResumeWindow(time.Hour),
		wspulse.WithSendBufferSize(bufferSize),
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
	)
	b.Cleanup(srv.Close)

	waitFor := func(ch <-chan struct{}, what string) {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			b.Fatalf("timed out waiting for %s", what)
		}
	}

	// Setup: connect, then drop the transport so the session enters
	// stateSuspended. PrefillResumeBuffer requires the session to exist;
	// DrainResumeBuffer doesn't care about state but won't run if the
	// session has already been removed.
	mt := newMockTransport()
	wspulse.InjectTransport(srv, connectionID, roomID, mt)
	waitFor(connected, "initial connect")
	mt.InjectError(errors.New("bench: transport closed"))
	waitFor(dropped, "transport drop")

	// Pre-encode the bench payload once; the inner loop reuses the
	// encoded bytes via ForcePush, so no codec cost is included.
	encoded, err := wspulse.JSONCodec.Encode(wspulse.Message{
		Event:   "bench",
		Payload: jsonPayload(64),
	})
	if err != nil {
		b.Fatalf("JSONCodec.Encode setup: %v", err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if err := wspulse.PrefillResumeBuffer(srv, connectionID, encoded, bufferSize); err != nil {
			b.Fatalf("PrefillResumeBuffer: %v", err)
		}
		if _, err := wspulse.DrainResumeBuffer(srv, connectionID); err != nil {
			b.Fatalf("DrainResumeBuffer: %v", err)
		}
	}
}
