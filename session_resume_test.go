package wspulse_test

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	wspulse "github.com/wspulse/server"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// connectAndDrop injects a mock transport, waits for onConnect, then kills it
// to trigger session suspension. Returns the dead transport.
func connectAndDrop(t *testing.T, srv wspulse.Server, connectionID, roomID string, connected, dropped chan struct{}) *mockTransport {
	t.Helper()
	mt := injectAndWait(t, srv, connectionID, roomID, connected)
	mt.InjectError(errors.New("transport closed"))
	select {
	case <-dropped:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for transport drop")
	}
	return mt
}

// reconnect injects a new mock transport for the same connectionID to trigger
// session resumption. Waits for onTransportRestore. Returns the new transport.
func reconnect(t *testing.T, srv wspulse.Server, connectionID, roomID string, restored chan struct{}) *mockTransport {
	t.Helper()
	mt2 := newMockTransport()
	wspulse.InjectTransport(srv, connectionID, roomID, mt2)
	select {
	case <-restored:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for transport restore")
	}
	return mt2
}

// ── Resume: reconnect within window ─────────────────────────────────────────

func TestResume_ReconnectWithinWindow(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 2)
	disconnected := make(chan struct{}, 1)
	dropped := make(chan struct{}, 1)
	restored := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5*time.Second),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnTransportDrop(func(_ wspulse.Connection, _ error) {
			select {
			case dropped <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			select {
			case restored <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	connectAndDrop(t, srv, "test-connection", "test-room", connected, dropped)

	// OnDisconnect should NOT fire during resume window.
	select {
	case <-disconnected:
		require.Fail(t, "OnDisconnect fired during resume window")
	default:
	}

	mt2 := reconnect(t, srv, "test-connection", "test-room", restored)

	// Verify the resumed connection is reachable.
	frame := wspulse.Frame{Event: "after-resume", Payload: []byte(`"ok"`)}
	require.NoError(t, srv.Send("test-connection", frame))
	w, ok := mt2.WaitWrite(time.Second)
	require.True(t, ok, "expected write to resumed connection")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "after-resume", f.Event)

	// OnDisconnect still should NOT have fired.
	select {
	case <-disconnected:
		require.Fail(t, "OnDisconnect fired after successful resume")
	default:
	}
}

// ── Resume: grace expires ───────────────────────────────────────────────────

func TestResume_GraceExpires_FiresOnDisconnect(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	dropped := make(chan struct{}, 1)
	fc := newFakeClock()

	srv := wspulse.NewServer(
		acceptAll,
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
	t.Cleanup(srv.Close)

	connectAndDrop(t, srv, "test-connection", "test-room", connected, dropped)

	// Verify grace timer registered with correct duration.
	require.Equal(t, 3*time.Minute, fc.Duration(0), "grace timer duration")

	// Fire the grace timer.
	fc.Fire(0)

	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnDisconnect after grace expiry")
	}
}

// ── Resume: buffered frames delivered ───────────────────────────────────────

func TestResume_BufferedFramesDelivered(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 2)
	dropped := make(chan struct{}, 1)
	restored := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5*time.Second),
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
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			select {
			case restored <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	connectAndDrop(t, srv, "test-connection", "test-room", connected, dropped)

	// Send frames while suspended — they go to ring buffer.
	for i := 0; i < 3; i++ {
		_ = srv.Send("test-connection", wspulse.Frame{
			Event:   "buffered",
			Payload: []byte(`"` + string(rune('a'+i)) + `"`),
		})
	}

	mt2 := reconnect(t, srv, "test-connection", "test-room", restored)

	// Read all 3 buffered frames.
	var frames []wspulse.Frame
	for i := 0; i < 3; i++ {
		w, ok := mt2.WaitWrite(time.Second)
		require.True(t, ok, "expected buffered frame %d", i)
		f, err := wspulse.JSONCodec.Decode(w.data)
		require.NoError(t, err)
		frames = append(frames, f)
	}
	require.Len(t, frames, 3)
	for _, f := range frames {
		assert.Equal(t, "buffered", f.Event)
	}
}

// ── Resume: kick bypasses window ────────────────────────────────────────────

func TestResume_KickBypassesWindow(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	dropped := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
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
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	connectAndDrop(t, srv, "test-connection", "test-room", connected, dropped)

	// Kick should bypass the resume window and fire OnDisconnect immediately.
	require.NoError(t, srv.Kick("test-connection"))

	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnDisconnect after Kick")
	}
}

// ── Resume: no resume window → disconnects immediately ──────────────────────

func TestResume_NoResumeWindow_DisconnectsImmediately(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		// No WithResumeWindow — default is 0 (disabled).
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			select {
			case connected <- struct{}{}:
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
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)
	mt.InjectError(errors.New("transport closed"))

	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnDisconnect (no resume window)")
	}
}

// ── Resume: server close terminates suspended ───────────────────────────────

func TestResume_ServerCloseTerminatesSuspended(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnected := make(chan error, 1)
	dropped := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
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
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, err error) {
			select {
			case disconnected <- err:
			default:
			}
		}),
	)

	connectAndDrop(t, srv, "test-connection", "test-room", connected, dropped)

	// Close server while session is suspended.
	srv.Close()

	select {
	case err := <-disconnected:
		assert.ErrorIs(t, err, wspulse.ErrServerClosed)
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnDisconnect from server close on suspended session")
	}
}

// ── Resume: broadcast while suspended ───────────────────────────────────────

func TestResume_BroadcastWhileSuspended(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 2)
	dropped := make(chan struct{}, 1)
	restored := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5*time.Second),
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
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			select {
			case restored <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	connectAndDrop(t, srv, "test-connection", "test-room", connected, dropped)

	// Broadcast while suspended — frames go to ring buffer.
	require.NoError(t, srv.Broadcast("test-room", wspulse.Frame{Event: "suspended-broadcast"}))

	mt2 := reconnect(t, srv, "test-connection", "test-room", restored)

	// Buffered broadcast should be delivered.
	w, ok := mt2.WaitWrite(time.Second)
	require.True(t, ok, "expected buffered broadcast frame")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "suspended-broadcast", f.Event)
}

// ── Resume: Connection.Close while suspended ────────────────────────────────

func TestResume_ConnectionCloseWhileSuspended(t *testing.T) {
	t.Parallel()
	connected := make(chan wspulse.Connection, 1)
	disconnected := make(chan struct{}, 1)
	dropped := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(10*time.Second),
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			select {
			case connected <- c:
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
	t.Cleanup(srv.Close)

	mt := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt)
	var conn wspulse.Connection
	select {
	case conn = <-connected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for connect")
	}

	// Drop transport to enter suspended state.
	mt.InjectError(errors.New("transport closed"))
	select {
	case <-dropped:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for transport drop")
	}

	// Close the connection while suspended — should fire OnDisconnect immediately.
	require.NoError(t, conn.Close())

	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnDisconnect after Connection.Close on suspended session")
	}
}

// ── Transport callbacks: drop fires on suspend ──────────────────────────────

func TestOnTransportDrop_FiresOnSuspend(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	dropErr := make(chan error, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5*time.Second),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnTransportDrop(func(_ wspulse.Connection, err error) {
			select {
			case dropErr <- err:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)
	mt.InjectError(errors.New("test: connection reset"))

	select {
	case <-dropErr:
		// Callback fired — error may be nil for normal close.
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnTransportDrop")
	}
}

// ── Transport callbacks: restore fires on resume ────────────────────────────

func TestOnTransportRestore_FiresOnResume(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 2)
	dropped := make(chan struct{}, 1)
	restored := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5*time.Second),
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
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			select {
			case restored <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	connectAndDrop(t, srv, "test-connection", "test-room", connected, dropped)
	reconnect(t, srv, "test-connection", "test-room", restored)
	// If we reach here, restore was fired successfully.
}

// ── Transport callbacks: not fired without resume window ────────────────────

func TestTransportCallbacks_NotFired_WithoutResumeWindow(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	dropFired := false
	restoreFired := false

	srv := wspulse.NewServer(
		acceptAll,
		// No resume window.
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnTransportDrop(func(_ wspulse.Connection, _ error) {
			dropFired = true
		}),
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			restoreFired = true
		}),
	)
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)
	mt.InjectError(errors.New("transport closed"))

	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnDisconnect")
	}

	assert.False(t, dropFired, "OnTransportDrop should not fire without resume window")
	assert.False(t, restoreFired, "OnTransportRestore should not fire without resume window")
}

// ── Resume: concurrent reconnect (race detector) ────────────────────────────

func TestResume_ConcurrentReconnect_NoRace(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	dropped := make(chan struct{}, 10)
	restored := make(chan struct{}, 10)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(2*time.Second),
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
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			select {
			case restored <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	for i := 0; i < 10; i++ {
		mt := newMockTransport()
		wspulse.InjectTransport(srv, "test-connection", "test-room", mt)

		if i == 0 {
			select {
			case <-connected:
			case <-time.After(time.Second):
				require.Failf(t, "timed out", "cycle %d: waiting for connect", i)
			}
		} else {
			select {
			case <-restored:
			case <-time.After(time.Second):
				require.Failf(t, "timed out", "cycle %d: waiting for transport restore", i)
			}
		}

		mt.InjectError(errors.New("transport closed"))
		select {
		case <-dropped:
		case <-time.After(time.Second):
			require.Failf(t, "timed out", "cycle %d: waiting for transport drop", i)
		}
	}
}

// ── Resume: concurrent broadcast during resume (race detector) ──────────────

func TestResume_ConcurrentBroadcastDuringResume_NoRace(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 2)
	dropped := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(2*time.Second),
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

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)

	// Start concurrent broadcasts, then close the transport to trigger
	// suspend. The race detector checks for data races between broadcast
	// fan-out and the hub's transport swap.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = srv.Broadcast("test-room", wspulse.Frame{Event: "ping"})
			}
		}()
	}

	mt.InjectError(errors.New("transport closed"))
	select {
	case <-dropped:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for transport drop")
	}

	// Reconnect while broadcasts are still in flight.
	mt2 := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt2)

	wg.Wait()
	// Drain any writes to avoid blocking.
	mt2.DrainWrites()
}

// ── Resume: stale closed session reconnect ──────────────────────────────────

func TestResume_StaleClosedSession_Reconnect(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 4)
	disconnected := make(chan struct{}, 4)
	dropped := make(chan struct{}, 4)
	fc := newFakeClock()

	srv := wspulse.NewServer(
		acceptAll,
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
	t.Cleanup(srv.Close)

	// First connection.
	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)

	// Close transport and fire the grace timer.
	mt.InjectError(errors.New("transport closed"))
	select {
	case <-dropped:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnTransportDrop")
	}

	fc.Fire(0)

	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for grace expiry disconnect")
	}

	// Reconnect with the same connectionID — hits stateClosed branch.
	mt2 := injectAndWait(t, srv, "test-connection", "test-room", connected)

	// Verify the new connection works.
	require.NoError(t, srv.Send("test-connection", wspulse.Frame{Event: "alive"}))
	w, ok := mt2.WaitWrite(time.Second)
	require.True(t, ok, "expected write to new connection")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "alive", f.Event)
}

// ── Resume: multiple rapid drop/reconnect cycles ────────────────────────────

func TestResume_MultipleRapidCycles(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	dropped := make(chan struct{}, 10)
	restored := make(chan struct{}, 10)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5*time.Second),
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
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			select {
			case restored <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	for i := 0; i < 5; i++ {
		mt := newMockTransport()
		wspulse.InjectTransport(srv, "test-connection", "test-room", mt)

		if i == 0 {
			select {
			case <-connected:
			case <-time.After(time.Second):
				require.Failf(t, "timed out", "cycle %d: waiting for connect", i)
			}
		} else {
			select {
			case <-restored:
			case <-time.After(time.Second):
				require.Failf(t, "timed out", "cycle %d: waiting for transport restore", i)
			}
		}

		mt.InjectError(errors.New("transport closed"))
		select {
		case <-dropped:
		case <-time.After(time.Second):
			require.Failf(t, "timed out", "cycle %d: waiting for transport drop", i)
		}
	}
}

// ── Resume: grace expires after Connection.Close ────────────────────────────

func TestResume_GraceExpiresAfterConnectionClose(t *testing.T) {
	t.Parallel()
	connected := make(chan wspulse.Connection, 1)
	dropped := make(chan struct{}, 1)
	fc := newFakeClock()

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(3*time.Minute),
		wspulse.WithClock(fc),
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			select {
			case connected <- c:
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

	mt := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt)
	var conn wspulse.Connection
	select {
	case conn = <-connected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for connect")
	}

	// Close transport to enter suspended state.
	mt.InjectError(errors.New("transport closed"))
	select {
	case <-dropped:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnTransportDrop")
	}

	// Application calls Close() on the Connection while suspended.
	require.NoError(t, conn.Close())

	// Fire the grace timer. handleGraceExpired will see stateClosed
	// and clean up maps without calling Close again.
	fc.Fire(0)

	// Poll until the session is removed from hub maps.
	deadline := time.Now().Add(time.Second)
	for {
		connections := srv.GetConnections("test-room")
		if len(connections) == 0 {
			break
		}
		if time.Now().After(deadline) {
			require.Failf(t, "want 0 connections after grace expired", "got %d", len(connections))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ── Resume: kick while connected, transport died handled ────────────────────

func TestResume_KickWhileConnected_TransportDiedHandled(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(10*time.Second),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			select {
			case connected <- struct{}{}:
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
	t.Cleanup(srv.Close)

	_ = injectAndWait(t, srv, "test-connection", "test-room", connected)

	require.NoError(t, srv.Kick("test-connection"))

	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnDisconnect")
	}
	// Let deferred cleanup finish.
	time.Sleep(50 * time.Millisecond)
}

// ── Resume: duplicate ID while connected kicks old ──────────────────────────

func TestResume_DuplicateID_WhileConnected_KicksOld(t *testing.T) {
	t.Parallel()
	var (
		firstConnected = make(chan struct{})
		kicked         = make(chan struct{})
		mu             sync.Mutex
		connectCount   int
	)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5*time.Second),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			mu.Lock()
			connectCount++
			n := connectCount
			mu.Unlock()
			if n == 1 {
				close(firstConnected)
			}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, err error) {
			if errors.Is(err, wspulse.ErrDuplicateConnectionID) {
				close(kicked)
			}
		}),
	)
	t.Cleanup(srv.Close)

	// First connection.
	_ = injectAndWait(t, srv, "test-connection", "test-room", firstConnected)

	// Second connection with same ID while first is still connected.
	mt2 := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt2)
	select {
	case <-kicked:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for duplicate kick")
	}
}

// ── Resume: writePump stops on pumpQuit ─────────────────────────────────────

func TestResume_WritePumpStopsOnPumpQuit(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 2)
	dropped := make(chan struct{}, 1)
	restored := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5*time.Second),
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
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			select {
			case restored <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	mt1 := injectAndWait(t, srv, "test-connection", "test-room", connected)

	// Send data before closing to ensure writePump is active.
	require.NoError(t, srv.Send("test-connection", wspulse.Frame{Event: "pre-suspend"}))
	w, ok := mt1.WaitWrite(time.Second)
	require.True(t, ok, "expected pre-suspend write")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "pre-suspend", f.Event)

	// Close transport to suspend, writePump gets pumpQuit.
	mt1.InjectError(errors.New("transport closed"))
	select {
	case <-dropped:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for transport drop")
	}

	// Reconnect — triggers attachWS, which waits for old pumpDone.
	mt2 := reconnect(t, srv, "test-connection", "test-room", restored)

	// If old writePump didn't exit via pumpQuit, the new pump wouldn't start.
	require.NoError(t, srv.Send("test-connection", wspulse.Frame{Event: "post-resume"}))
	w2, ok := mt2.WaitWrite(time.Second)
	require.True(t, ok, "expected post-resume write")
	f2, err := wspulse.JSONCodec.Decode(w2.data)
	require.NoError(t, err)
	assert.Equal(t, "post-resume", f2.Event)
}

// ── Resume: writePump exits via pumpQuit (long ping) ────────────────────────

func TestResume_WritePumpExitsViaPumpQuit(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 2)
	dropped := make(chan struct{}, 1)
	restored := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5*time.Second),
		wspulse.WithHeartbeat(5*time.Second, 30*time.Second), // long ping period
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
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			select {
			case restored <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	mt1 := injectAndWait(t, srv, "test-connection", "test-room", connected)

	// Close client transport to trigger suspend. With long ping period,
	// writePump won't attempt any writes before pumpQuit fires.
	mt1.InjectError(errors.New("transport closed"))
	select {
	case <-dropped:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for transport drop")
	}

	// Reconnect to prove the old writePump has exited correctly.
	mt2 := reconnect(t, srv, "test-connection", "test-room", restored)

	require.NoError(t, srv.Send("test-connection", wspulse.Frame{Event: "verify"}))
	w, ok := mt2.WaitWrite(time.Second)
	require.True(t, ok, "expected verify write")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "verify", f.Event)
}

// ── Resume: Connection.Close while suspended then reconnect ─────────────────

func TestResume_ConnectionCloseWhileSuspended_ThenReconnect(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 4)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(10*time.Second),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)

	// Close transport to enter suspended state.
	mt.InjectError(errors.New("transport closed"))
	time.Sleep(50 * time.Millisecond) // let hub process transportDied

	// Get the connection reference and call Close() on it.
	connections := srv.GetConnections("test-room")
	require.Len(t, connections, 1)
	require.NoError(t, connections[0].Close())
	time.Sleep(50 * time.Millisecond)

	// Reconnect with the same connectionID — handleRegister sees stateClosed,
	// cleans up stale entry, creates new session.
	mt2 := injectAndWait(t, srv, "test-connection", "test-room", connected)

	// Verify new session works.
	require.NoError(t, srv.Send("test-connection", wspulse.Frame{Event: "after-stale"}))
	w, ok := mt2.WaitWrite(time.Second)
	require.True(t, ok, "expected write to new connection")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "after-stale", f.Event)
}

// ── Resume: stale closed session fires OnDisconnect ─────────────────────────

func TestResume_StaleClosedSession_OnDisconnectFires(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 4)
	disconnected := make(chan struct{}, 4)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(60*time.Second),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			select {
			case connected <- struct{}{}:
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
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "test-connection", "test-room", connected)

	// Suspend the session by dropping the transport.
	mt.InjectError(errors.New("transport closed"))
	time.Sleep(50 * time.Millisecond)

	// Call Close() on the suspended session.
	conns := srv.GetConnections("test-room")
	if len(conns) > 0 {
		_ = conns[0].Close()
	}

	// Reconnect immediately — races the graceExpiredMessage.
	mt2 := injectAndWait(t, srv, "test-connection", "test-room", connected)

	// The old session's onDisconnect should fire.
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for onDisconnect from stale session")
	}

	// Verify new session works.
	require.NoError(t, srv.Send("test-connection", wspulse.Frame{Event: "alive"}))
	w, ok := mt2.WaitWrite(time.Second)
	require.True(t, ok, "expected write to new connection")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "alive", f.Event)
}

// ── Resume: drain buffer full ───────────────────────────────────────────────

func TestResume_DrainBufferFull(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 2)
	dropped := make(chan struct{}, 1)
	restored := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithSendBufferSize(1),
		wspulse.WithResumeWindow(5*time.Second),
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
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			select {
			case restored <- struct{}{}:
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
	t.Cleanup(srv.Close)

	connectAndDrop(t, srv, "test-connection", "test-room", connected, dropped)

	// Send multiple frames while suspended — they go to the resume buffer.
	// With send buffer size 1, draining 3 frames overflows the send channel
	// and triggers the drop-oldest backpressure in the drain loop.
	for i := 0; i < 3; i++ {
		_ = srv.Send("test-connection", wspulse.Frame{
			Event:   "buffered",
			Payload: []byte(`"` + string(rune('a'+i)) + `"`),
		})
	}

	// Reconnect — the transition goroutine drains the resume buffer.
	mt2 := reconnect(t, srv, "test-connection", "test-room", restored)

	// Read at least one frame to verify the session is functional.
	w, ok := mt2.WaitWrite(time.Second)
	require.True(t, ok, "expected at least one buffered frame")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "buffered", f.Event)

	// Session should still be alive.
	select {
	case <-disconnected:
		require.Fail(t, "onDisconnect should not fire after successful resume")
	default:
	}
}

// ── Resume: Connection.Close while suspended fires OnDisconnect ─────────────

func TestResume_ConnectionCloseWhileSuspended_FiresOnDisconnect(t *testing.T) {
	t.Parallel()
	connected := make(chan wspulse.Connection, 1)
	disconnected := make(chan struct{})
	var once sync.Once

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(time.Second),
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			select {
			case connected <- c:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			once.Do(func() { close(disconnected) })
		}),
	)
	t.Cleanup(srv.Close)

	mt := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt)
	var conn wspulse.Connection
	select {
	case conn = <-connected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for connect")
	}

	// Drop the transport — session enters suspended state.
	mt.InjectError(errors.New("transport closed"))
	time.Sleep(50 * time.Millisecond) // let hub process transportDied

	// Confirm the session is still registered (suspended).
	require.NotEmpty(t, srv.GetConnections("test-room"),
		"session was removed before grace window elapsed")

	// Application calls Close() on the suspended Connection.
	_ = conn.Close()

	// onDisconnect must fire.
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "onDisconnect did not fire after Connection.Close() on suspended session")
	}

	// Session must be removed from hub maps after onDisconnect.
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, srv.GetConnections("test-room"),
		"want 0 connections after Close + grace expiry")
}

// ── Resume: Connection.Close fires OnDisconnect immediately ─────────────────

func TestResume_ConnectionClose_ImmediateOnDisconnect(t *testing.T) {
	t.Parallel()
	connected := make(chan wspulse.Connection, 1)
	disconnected := make(chan struct{})
	var once sync.Once

	const gracePeriod = 5 * time.Second

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(gracePeriod),
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			select {
			case connected <- c:
			default:
			}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			once.Do(func() { close(disconnected) })
		}),
	)
	t.Cleanup(srv.Close)

	mt := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt)
	var conn wspulse.Connection
	select {
	case conn = <-connected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for connect")
	}

	// Drop the transport — session enters suspended state.
	mt.InjectError(errors.New("transport closed"))
	time.Sleep(50 * time.Millisecond) // let hub process transportDied

	// Application calls Close() on the suspended session.
	start := time.Now()
	_ = conn.Close()

	// onDisconnect must fire promptly, not after the full grace window.
	const wantWithin = 500 * time.Millisecond
	select {
	case <-disconnected:
		require.Less(t, time.Since(start), wantWithin,
			"onDisconnect took too long after Connection.Close()")
	case <-time.After(gracePeriod):
		require.Fail(t, "onDisconnect fired after full grace window")
	}

	// Session must be removed from hub maps after onDisconnect.
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, srv.GetConnections("test-room"),
		"want 0 connections after Close")
}

// ── Resume: mass close while suspended, all OnDisconnect fire ───────────────

func TestResume_MassCloseWhileSuspended_AllOnDisconnect(t *testing.T) {
	t.Parallel()
	const count = 50 // reduced from 200 for component tests

	var mu sync.Mutex
	connections := make([]wspulse.Connection, 0, count)
	allConnected := make(chan struct{})
	var disconnectCount int64
	allDisconnected := make(chan struct{})

	connIdx := 0
	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			mu.Lock()
			connIdx++
			id := fmt.Sprintf("conn-%d", connIdx)
			mu.Unlock()
			return "room", id, nil
		},
		wspulse.WithResumeWindow(30*time.Second),
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			mu.Lock()
			connections = append(connections, c)
			n := len(connections)
			mu.Unlock()
			if n == count {
				close(allConnected)
			}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			if atomic.AddInt64(&disconnectCount, 1) == int64(count) {
				close(allDisconnected)
			}
		}),
	)
	t.Cleanup(srv.Close)

	// Create count connections.
	mts := make([]*mockTransport, count)
	for i := 0; i < count; i++ {
		mt := newMockTransport()
		mts[i] = mt
		mu.Lock()
		id := fmt.Sprintf("conn-%d", connIdx+1)
		mu.Unlock()
		_ = id
		wspulse.InjectTransport(srv, fmt.Sprintf("conn-%d", i+1), "room", mt)
	}

	select {
	case <-allConnected:
	case <-time.After(5 * time.Second):
		require.Fail(t, "timed out waiting for all connections")
	}

	// Drop all transports to enter suspended state.
	for _, mt := range mts {
		mt.InjectError(errors.New("transport closed"))
	}
	time.Sleep(200 * time.Millisecond) // let hub process all transportDied messages

	// Close all suspended sessions concurrently.
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
		got := atomic.LoadInt64(&disconnectCount)
		require.Failf(t, "onDisconnect count mismatch",
			"want %d, got %d (lost %d)", count, got, int64(count)-got)
	}
}

// ── Resume: close races with transport died ─────────────────────────────────

func TestResume_CloseRacesTransportDied(t *testing.T) {
	t.Parallel()
	const count = 50

	var mu sync.Mutex
	connections := make([]wspulse.Connection, 0, count)
	transports := make([]*mockTransport, 0, count)
	allConnected := make(chan struct{})
	var disconnectCount int64
	allDisconnected := make(chan struct{})

	connIdx := 0
	srv := wspulse.NewServer(
		func(r *http.Request) (string, string, error) {
			return "room", "", nil
		},
		wspulse.WithResumeWindow(30*time.Second),
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			mu.Lock()
			connections = append(connections, c)
			n := len(connections)
			mu.Unlock()
			if n == count {
				close(allConnected)
			}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			if atomic.AddInt64(&disconnectCount, 1) == int64(count) {
				close(allDisconnected)
			}
		}),
	)
	t.Cleanup(srv.Close)

	for i := 0; i < count; i++ {
		mt := newMockTransport()
		mu.Lock()
		transports = append(transports, mt)
		connIdx++
		id := fmt.Sprintf("conn-%d", connIdx)
		mu.Unlock()
		wspulse.InjectTransport(srv, id, "room", mt)
	}

	select {
	case <-allConnected:
	case <-time.After(5 * time.Second):
		require.Fail(t, "timed out waiting for all connections")
	}

	// For each pair: close the transport and call Close() back-to-back
	// with NO sleep in between, so Close() may execute before the hub
	// has finished assigning graceTimer in handleTransportDied.
	mu.Lock()
	snapshot := make([]wspulse.Connection, len(connections))
	copy(snapshot, connections)
	tSnapshot := make([]*mockTransport, len(transports))
	copy(tSnapshot, transports)
	mu.Unlock()

	var wg sync.WaitGroup
	for i, conn := range snapshot {
		wg.Add(1)
		go func(c wspulse.Connection, mt *mockTransport) {
			defer wg.Done()
			mt.InjectError(errors.New("transport closed"))
			_ = c.Close()
		}(conn, tSnapshot[i])
	}
	wg.Wait()

	select {
	case <-allDisconnected:
	case <-time.After(3 * time.Second):
		got := atomic.LoadInt64(&disconnectCount)
		require.Failf(t, "onDisconnect delayed by grace window",
			"want %d within 3s, got %d", count, got)
	}
}

// ── Resume: grace timer fires after reconnect (stale, stateConnected) ───────

func TestResume_GraceTimerFiresAfterReconnect(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	dropped := make(chan struct{}, 1)
	restored := make(chan struct{}, 1)
	fc := newFakeClock()

	srv := wspulse.NewServer(
		acceptAll,
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
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			select {
			case restored <- struct{}{}:
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
	t.Cleanup(srv.Close)

	// First connection.
	connectAndDrop(t, srv, "test-connection", "test-room", connected, dropped)

	// Reconnect before firing the grace timer.
	mt2 := reconnect(t, srv, "test-connection", "test-room", restored)

	// Fire the grace timer — session is already resumed (stateConnected),
	// so handleGraceExpired should skip it.
	fc.Fire(0)

	// Verify session is still alive by round-tripping a frame.
	frame := wspulse.Frame{Event: "still-alive", Payload: []byte(`"ok"`)}
	require.NoError(t, srv.Send("test-connection", frame))
	w, ok := mt2.WaitWrite(time.Second)
	require.True(t, ok, "expected write after grace timer expired")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "still-alive", f.Event)

	select {
	case <-disconnected:
		require.Fail(t, "onDisconnect should not fire — session was resumed before timer")
	default:
	}
}

// ── Resume: stale grace timer (epoch mismatch) ──────────────────────────────

func TestResume_StaleGraceTimer(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 10)
	disconnected := make(chan struct{}, 10)
	dropped := make(chan struct{}, 10)
	restored := make(chan struct{}, 10)
	fc := newFakeClock()

	srv := wspulse.NewServer(
		acceptAll,
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
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			select {
			case restored <- struct{}{}:
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
	t.Cleanup(srv.Close)

	// Cycle 1: connect and drop. Timer 0 (epoch=1).
	connectAndDrop(t, srv, "test-connection", "test-room", connected, dropped)

	// Cycle 2: reconnect (resume), then drop again. Timer 1 (epoch=2).
	mt2 := reconnect(t, srv, "test-connection", "test-room", restored)
	mt2.InjectError(errors.New("transport closed"))
	select {
	case <-dropped:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for second drop")
	}

	// Cycle 3: reconnect again (resume).
	mt3 := reconnect(t, srv, "test-connection", "test-room", restored)

	// Fire old timers (epoch=1, epoch=2) — both should be detected as stale.
	fc.Fire(0)
	fc.Fire(1)

	// Verify session is still alive by round-tripping a frame.
	require.NoError(t, srv.Send("test-connection", wspulse.Frame{Event: "alive"}))
	w, ok := mt3.WaitWrite(time.Second)
	require.True(t, ok, "expected write after stale timers")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "alive", f.Event)

	// After round-trip, verify OnDisconnect was not fired.
	select {
	case <-disconnected:
		require.Fail(t, "OnDisconnect fired — stale timer was not correctly ignored")
	default:
	}
}

// ── Connection.Close stateClosed then transport dies ─────────────────────────

func TestConnectionClose_StateClosed_TransportDied(t *testing.T) {
	t.Parallel()
	connected := make(chan wspulse.Connection, 1)
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			select {
			case connected <- c:
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
	t.Cleanup(srv.Close)

	mt := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt)
	var conn wspulse.Connection
	select {
	case conn = <-connected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for connect")
	}

	// Close the session externally — this sets state to stateClosed
	// and closes done. writePump sees done, closes transport.
	// readPump gets a read error and sends transportDied.
	_ = conn.Close()

	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for onDisconnect after Connection.Close()")
	}
}

// ── Connection.Close stateClosed then resume attempt ────────────────────────

func TestConnectionClose_StateClosed_Resume(t *testing.T) {
	t.Parallel()
	connected := make(chan wspulse.Connection, 2)
	disconnected := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5*time.Second),
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			select {
			case connected <- c:
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
	t.Cleanup(srv.Close)

	mt := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt)
	var conn wspulse.Connection
	select {
	case conn = <-connected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for connect")
	}

	// Close externally — state becomes stateClosed before transport dies.
	_ = conn.Close()

	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for onDisconnect")
	}
}

// ── OnTransportDrop: grace expires then disconnect ──────────────────────────

func TestOnTransportDrop_GraceExpires_ThenDisconnect(t *testing.T) {
	t.Parallel()
	events := make(chan string, 4)
	fc := newFakeClock()

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(3*time.Minute),
		wspulse.WithClock(fc),
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			events <- "connect"
		}),
		wspulse.WithOnTransportDrop(func(_ wspulse.Connection, _ error) {
			events <- "drop"
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			events <- "disconnect"
		}),
	)
	t.Cleanup(srv.Close)

	mt := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt)

	select {
	case e := <-events:
		require.Equal(t, "connect", e, "first event")
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for connect event")
	}

	mt.InjectError(errors.New("transport closed"))

	// Expect: drop fires first, then disconnect fires after grace expires.
	select {
	case e := <-events:
		require.Equal(t, "drop", e, "second event")
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for drop event")
	}

	fc.Fire(0)

	select {
	case e := <-events:
		require.Equal(t, "disconnect", e, "third event")
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for disconnect event")
	}
}

// ── OnTransportRestore: then OnMessage ──────────────────────────────────────

func TestOnTransportRestore_ThenOnMessage(t *testing.T) {
	t.Parallel()
	events := make(chan string, 4)
	connected := make(chan struct{}, 2)
	dropped := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5*time.Second),
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
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			events <- "restore"
		}),
		wspulse.WithOnMessage(func(_ wspulse.Connection, _ wspulse.Frame) {
			events <- "message"
		}),
	)
	t.Cleanup(srv.Close)

	connectAndDrop(t, srv, "test-connection", "test-room", connected, dropped)

	// Reconnect.
	restored := make(chan struct{}, 1)
	// We need a restore signal. Use events channel.
	mt2 := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt2)

	// Wait for restore event first.
	select {
	case e := <-events:
		require.Equal(t, "restore", e, "first event after reconnect")
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for restore event")
	}
	_ = restored // not used here

	// Now send a message on the new transport.
	encoded, err := wspulse.JSONCodec.Encode(wspulse.Frame{Event: "ping"})
	require.NoError(t, err)
	mt2.InjectMessage(wspulse.TextMessage, encoded)

	select {
	case e := <-events:
		require.Equal(t, "message", e, "second event after reconnect")
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for message event")
	}
}

// ── OnTransportRestore: fires after state connected ─────────────────────────

func TestOnTransportRestore_FiresAfterStateConnected(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 2)
	dropped := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5*time.Second),
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
		wspulse.WithOnTransportRestore(func(c wspulse.Connection) {
			// Send a frame from within the callback. If this fires
			// before stateConnected, the frame goes to the resume
			// buffer (already drained) and never reaches the client.
			_ = c.Send(wspulse.Frame{
				Event:   "from-restore",
				Payload: []byte(`"hello"`),
			})
		}),
	)
	t.Cleanup(srv.Close)

	connectAndDrop(t, srv, "test-connection", "test-room", connected, dropped)

	// Reconnect with same connectionID.
	mt2 := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt2)

	// The client must receive the frame sent from OnTransportRestore.
	w, ok := mt2.WaitWrite(time.Second)
	require.True(t, ok, "expected write from OnTransportRestore callback")
	f, err := wspulse.JSONCodec.Decode(w.data)
	require.NoError(t, err)
	assert.Equal(t, "from-restore", f.Event)
}

// ── OnTransportRestore: not fired on closed session ─────────────────────────

func TestOnTransportRestore_NotFiredOnClosedSession(t *testing.T) {
	t.Parallel()
	connected := make(chan wspulse.Connection, 2)
	dropped := make(chan struct{}, 1)
	restoreFired := make(chan struct{}, 1)

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5*time.Second),
		wspulse.WithOnConnect(func(c wspulse.Connection) {
			select {
			case connected <- c:
			default:
			}
		}),
		wspulse.WithOnTransportDrop(func(c wspulse.Connection, _ error) {
			// Close the session from the drop callback. This sets
			// state = stateClosed while the session is still suspended.
			_ = c.Close()
			select {
			case dropped <- struct{}{}:
			default:
			}
		}),
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) {
			select {
			case restoreFired <- struct{}{}:
			default:
			}
		}),
	)
	t.Cleanup(srv.Close)

	mt := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt)
	select {
	case <-connected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for first connect")
	}

	// Drop transport — session suspends, OnTransportDrop fires and calls Close().
	mt.InjectError(errors.New("transport closed"))
	select {
	case <-dropped:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnTransportDrop")
	}

	// Reconnect with the same connectionID immediately. Depending on
	// hub event ordering, the old closed session may or may not still
	// be registered. Either way, OnTransportRestore must NOT fire for
	// a session that was closed.
	mt2 := newMockTransport()
	wspulse.InjectTransport(srv, "test-connection", "test-room", mt2)

	select {
	case <-restoreFired:
		require.Fail(t, "OnTransportRestore fired on a closed session")
	case <-time.After(200 * time.Millisecond):
		// OK — callback did not fire for the closed session.
	}
}
