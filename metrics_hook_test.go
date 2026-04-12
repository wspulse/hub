package wspulse_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	wspulse "github.com/wspulse/hub"
)

// ── recordingCollector helpers (moved from metrics_integration_test.go) ──────

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

// ── Metrics: connection lifecycle ───────────────────────────────────────────

func TestMetricsCollector_ConnectionLifecycle(t *testing.T) {
	t.Parallel()
	rec := &recordingCollector{}
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewHub(
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
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "metrics-conn", "metrics-room", connected)

	assert.Equal(t, 1, rec.countByName("RoomCreated"), "RoomCreated")
	assert.Equal(t, 1, rec.countByName("ConnectionOpened"), "ConnectionOpened")

	// Kill transport.
	mt.InjectError(errors.New("closed"))
	requireReceive(t, disconnected)

	assert.Equal(t, 1, rec.countByName("ConnectionClosed"), "ConnectionClosed")
	assert.Equal(t, 1, rec.countByName("RoomDestroyed"), "RoomDestroyed")

	for _, e := range rec.eventsByName("ConnectionClosed") {
		assert.GreaterOrEqual(t, e.duration, time.Duration(0), "duration >= 0")
		assert.Equal(t, wspulse.DisconnectNormal, e.reason, "reason")
	}
}

// ── Metrics: message flow ───────────────────────────────────────────────────

func TestMetricsCollector_MessageFlow(t *testing.T) {
	t.Parallel()
	rec := &recordingCollector{}
	connected := make(chan struct{}, 4)
	broadcastDone := make(chan struct{}, 1)

	connIndex := 0
	var srv wspulse.Hub
	srv = wspulse.NewHub(
		func(r *http.Request) (string, string, error) {
			connIndex++
			return "metrics-room", fmt.Sprintf("conn-%d", connIndex), nil
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
	t.Cleanup(srv.Close)

	// Connect 2 clients.
	mt1 := injectAndWait(t, srv, "conn-1", "metrics-room", connected)
	_ = injectAndWait(t, srv, "conn-2", "metrics-room", connected)

	// Inject a message from conn-1.
	encoded, _ := wspulse.JSONCodec.Encode(wspulse.Frame{Event: "test"})
	mt1.InjectMessage(wspulse.TextMessage, encoded)

	requireReceive(t, broadcastDone)

	// Wait for MessageSent to fire for both connections.
	deadline := time.Now().Add(time.Second)
	for rec.countByName("MessageSent") < 2 {
		if time.Now().After(deadline) {
			require.Failf(t, "timed out", "MessageSent=%d (want >= 2)", rec.countByName("MessageSent"))
		}
		time.Sleep(5 * time.Millisecond)
	}

	assert.Equal(t, 1, rec.countByName("MessageReceived"), "MessageReceived")
	assert.Equal(t, 1, rec.countByName("MessageBroadcast"), "MessageBroadcast")
}

// ── Metrics: resume attempt ─────────────────────────────────────────────────

func TestMetricsCollector_ResumeAttempt(t *testing.T) {
	t.Parallel()
	rec := &recordingCollector{}
	connected := make(chan struct{}, 2)
	dropped := make(chan struct{}, 1)
	restored := make(chan struct{}, 1)

	srv := wspulse.NewHub(
		func(r *http.Request) (string, string, error) {
			return "resume-room", "resume-conn", nil
		},
		wspulse.WithMetrics(rec),
		wspulse.WithClock(newFakeClock()),
		wspulse.WithResumeWindow(5*time.Second),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnTransportDrop(func(_ wspulse.Connection, _ error) {
			dropped <- struct{}{}
		}),
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			restored <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	connectAndDrop(t, srv, "resume-conn", "resume-room", connected, dropped)

	assert.Equal(t, 1, rec.countByName("ConnectionOpened"), "ConnectionOpened")

	// Reconnect.
	reconnect(t, srv, "resume-conn", "resume-room", restored)

	assert.Equal(t, 1, rec.countByName("ResumeAttempt"), "ResumeAttempt")
	assert.Equal(t, 1, rec.countByName("ConnectionOpened"), "ConnectionOpened after resume (should not increment)")
}

// ── Metrics: frame dropped (send buffer full) ───────────────────────────────

func TestMetricsCollector_FrameDropped_SendFull(t *testing.T) {
	t.Parallel()
	rec := &recordingCollector{}
	connected := make(chan wspulse.Connection, 1)

	srv := wspulse.NewHub(
		func(r *http.Request) (string, string, error) {
			return "drop-room", "drop-conn", nil
		},
		wspulse.WithMetrics(rec),
		wspulse.WithSendBufferSize(1),
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			connected <- c
		}),
	)
	t.Cleanup(srv.Close)

	mt := newMockTransport()
	wspulse.InjectTransport(srv, "drop-conn", "drop-room", mt)
	conn := requireReceive(t, connected)

	frame := wspulse.Frame{Event: "fill", Payload: []byte(`{}`)}
	var gotBufferFull bool
	for i := 0; i < 200; i++ {
		if sendErr := conn.Send(frame); sendErr == wspulse.ErrSendBufferFull {
			gotBufferFull = true
			break
		}
	}
	require.True(t, gotBufferFull, "expected ErrSendBufferFull")
	assert.GreaterOrEqual(t, rec.countByName("FrameDropped"), 1, "FrameDropped")
}

// ── Metrics: frame dropped (broadcast drop-oldest while suspended) ──────────

func TestMetricsCollector_FrameDropped_BroadcastDropOldest(t *testing.T) {
	t.Parallel()
	rec := &recordingCollector{}
	connected := make(chan struct{}, 1)
	dropped := make(chan struct{}, 1)

	srv := wspulse.NewHub(
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
			case dropped <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	connectAndDrop(t, srv, "drop-conn", "drop-room", connected, dropped)

	frame := wspulse.Frame{Event: "fill", Payload: []byte(`{}`)}
	_ = srv.Broadcast("drop-room", frame) // fills ring buffer (size 1)
	_ = srv.Broadcast("drop-room", frame) // triggers drop-oldest

	// Poll for FrameDropped.
	deadline := time.Now().Add(time.Second)
	for rec.countByName("FrameDropped") < 1 {
		if time.Now().After(deadline) {
			require.Failf(t, "timed out", "FrameDropped=%d", rec.countByName("FrameDropped"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ── Metrics: pong timeout ───────────────────────────────────────────────────
// PongTimeout is fired by pingPump when transport.Ping() fails. Use a short
// pingInterval and feed an error into the mock's pingErr channel.

func TestMetricsCollector_PongTimeout(t *testing.T) {
	t.Parallel()
	rec := &recordingCollector{}
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewHub(
		func(r *http.Request) (string, string, error) {
			return "timeout-room", "timeout-conn", nil
		},
		wspulse.WithMetrics(rec),
		wspulse.WithPingInterval(50*time.Millisecond),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			disconnected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "timeout-conn", "timeout-room", connected)

	// Feed a ping failure to simulate pong timeout.
	mt.pingErr <- errors.New("pong timeout")

	requireReceive(t, disconnected)

	assert.Equal(t, 1, rec.countByName("PongTimeout"), "PongTimeout")
}

// ── Metrics: shutdown ───────────────────────────────────────────────────────

func TestMetricsCollector_Shutdown(t *testing.T) {
	t.Parallel()
	rec := &recordingCollector{}
	connected := make(chan struct{}, 4)

	srv := wspulse.NewHub(
		func(r *http.Request) (string, string, error) {
			return "shutdown-room", "", nil
		},
		wspulse.WithMetrics(rec),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	_ = injectAndWait(t, srv, "conn-1", "shutdown-room", connected)
	_ = injectAndWait(t, srv, "conn-2", "shutdown-room", connected)

	srv.Close()

	closedEvents := rec.eventsByName("ConnectionClosed")
	require.Len(t, closedEvents, 2, "ConnectionClosed")
	for _, e := range closedEvents {
		assert.Equal(t, wspulse.DisconnectHubClose, e.reason, "reason")
	}
	assert.Equal(t, 1, rec.countByName("RoomDestroyed"), "RoomDestroyed")
}
