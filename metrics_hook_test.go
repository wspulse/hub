package wspulse_test

import (
	"context"
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
		wspulse.WithOnMessage(func(conn wspulse.Connection, f wspulse.Message) {
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
	encoded, _ := wspulse.JSONCodec.Encode(wspulse.Message{Event: "test"})
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

// ── Metrics: message dropped (send buffer full) ─────────────────────────────

func TestMetricsCollector_MessageDropped_SendFull(t *testing.T) {
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

	msg := wspulse.Message{Event: "fill", Payload: []byte(`{}`)}
	var gotBufferFull bool
	for i := 0; i < 200; i++ {
		if sendErr := conn.Send(msg); sendErr == wspulse.ErrSendBufferFull {
			gotBufferFull = true
			break
		}
	}
	require.True(t, gotBufferFull, "expected ErrSendBufferFull")
	assert.GreaterOrEqual(t, rec.countByName("MessageDropped"), 1, "MessageDropped")
}

// ── Metrics: message dropped (broadcast drop-oldest while suspended) ────────

func TestMetricsCollector_MessageDropped_BroadcastDropOldest(t *testing.T) {
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

	msg := wspulse.Message{Event: "fill", Payload: []byte(`{}`)}
	_ = srv.Broadcast("drop-room", msg) // fills ring buffer (size 1)
	_ = srv.Broadcast("drop-room", msg) // triggers drop-oldest

	// Poll for MessageDropped.
	deadline := time.Now().Add(time.Second)
	for rec.countByName("MessageDropped") < 1 {
		if time.Now().After(deadline) {
			require.Failf(t, "timed out", "MessageDropped=%d", rec.countByName("MessageDropped"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ── Metrics: broadcast counter excludes mid-broadcast closed sessions ───────
//
// Regression test for issue wspulse/hub#63. handleBroadcast iterates the room
// snapshot and calls target.enqueue for each session. The `<-target.done`
// soft check at the top of the loop only catches sessions whose done channel
// is already closed; if a session's send queue closes between the check and
// the enqueue call, enqueue returns ErrConnectionClosed but the
// MessageBroadcast metric used to count the recipient anyway. After the fix,
// the counter only ticks up when enqueue succeeds.

func TestMetricsCollector_MessageBroadcast_ExcludesClosedSession(t *testing.T) {
	t.Parallel()
	rec := &recordingCollector{}
	connected := make(chan struct{}, 3)

	connIndex := 0
	srv := wspulse.NewHub(
		func(_ *http.Request) (string, string, error) {
			connIndex++
			return "broadcast-room", fmt.Sprintf("conn-%d", connIndex), nil
		},
		wspulse.WithMetrics(rec),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	// Inject three sessions in the same room. conn-2 has BlockClose set
	// so the writePump's deferred CloseNow (after we close its send queue)
	// does not tear the session down — readPump stays blocked on the mock
	// transport, session.done stays open, and the session remains in the
	// hub's room map for the broadcast to iterate.
	mt1 := newMockTransport()
	mt2 := newMockTransport()
	mt3 := newMockTransport()
	mt2.SetBlockClose(true)

	wspulse.InjectTransport(srv, "conn-1", "broadcast-room", mt1)
	wspulse.InjectTransport(srv, "conn-2", "broadcast-room", mt2)
	wspulse.InjectTransport(srv, "conn-3", "broadcast-room", mt3)
	requireReceive(t, connected)
	requireReceive(t, connected)
	requireReceive(t, connected)

	// Force the race: close conn-2's send queue while leaving its done
	// channel open. <-target.done falls through to default in
	// handleBroadcast; target.enqueue returns ErrConnectionClosed.
	require.NoError(t, wspulse.CloseSessionSendQueue(srv, "conn-2"))

	require.NoError(t, srv.Broadcast("broadcast-room", wspulse.Message{Event: "ping", Payload: []byte(`{}`)}))

	// Wait for the heart goroutine to record the broadcast.
	deadline := time.Now().Add(time.Second)
	for rec.countByName("MessageBroadcast") < 1 {
		if time.Now().After(deadline) {
			require.Failf(t, "timed out", "MessageBroadcast not recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}

	events := rec.eventsByName("MessageBroadcast")
	require.Len(t, events, 1)
	assert.Equal(t, 2, events[0].fanOut, "broadcast counter must exclude session whose enqueue failed")
}

// ── Metrics: heartbeat failed ────────────────────────────────────────────────
// HeartbeatFailed is fired by pingPump when transport.Ping() fails. Install a
// ping handler that blocks until signalled, then returns an error.

func TestMetricsCollector_HeartbeatFailed(t *testing.T) {
	t.Parallel()
	rec := &recordingCollector{}
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)

	// Gate channel: the ping handler blocks until this is closed.
	failPing := make(chan struct{})

	srv := wspulse.NewHub(
		func(r *http.Request) (string, string, error) {
			return "timeout-room", "timeout-conn", nil
		},
		wspulse.WithMetrics(rec),
		wspulse.WithPingInterval(50*time.Millisecond),
		wspulse.WithWriteTimeout(25*time.Millisecond),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			disconnected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	mt := newMockTransport()
	// Ping succeeds until failPing is closed, then returns an error.
	mt.SetPingHandler(func(ctx context.Context) error {
		select {
		case <-failPing:
			return errors.New("pong timeout")
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	wspulse.InjectTransport(srv, "timeout-conn", "timeout-room", mt)
	requireReceive(t, connected)

	// Trigger the ping failure.
	close(failPing)

	requireReceive(t, disconnected)

	assert.Equal(t, 1, rec.countByName("HeartbeatFailed"), "HeartbeatFailed")
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
