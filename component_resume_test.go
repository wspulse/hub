package wspulse_test

import (
	"errors"
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

func TestComponent_Resume_ReconnectWithinWindow(t *testing.T) {
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

func TestComponent_Resume_GraceExpires_FiresOnDisconnect(t *testing.T) {
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

func TestComponent_Resume_BufferedFramesDelivered(t *testing.T) {
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

func TestComponent_Resume_KickBypassesWindow(t *testing.T) {
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

func TestComponent_Resume_NoResumeWindow_DisconnectsImmediately(t *testing.T) {
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

func TestComponent_Resume_ServerCloseTerminatesSuspended(t *testing.T) {
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

func TestComponent_Resume_BroadcastWhileSuspended(t *testing.T) {
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

func TestComponent_Resume_ConnectionCloseWhileSuspended(t *testing.T) {
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

// ── Resume: stale grace timer ignored ───────────────────────────────────────

func TestComponent_Resume_StaleGraceTimerIgnored(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 2)
	dropped := make(chan struct{}, 2)
	restored := make(chan struct{}, 2)
	disconnected := make(chan struct{}, 1)
	fc := newFakeClock()

	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithResumeWindow(5*time.Minute),
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

	// Cycle 1: connect → drop → resume.
	connectAndDrop(t, srv, "test-connection", "test-room", connected, dropped)
	reconnect(t, srv, "test-connection", "test-room", restored)

	// Cycle 2: drop → resume again.
	// Need to get the current transport and kill it.
	connections := srv.GetConnections("test-room")
	require.Len(t, connections, 1)

	// Inject error into the second transport's readPump.
	// The second mock transport is the one returned by reconnect.
	// Since we can't access it directly, we'll drop via a new InjectTransport pattern.
	// Actually, let's just do another drop+restore cycle by injecting a third transport.

	// Drop cycle 2: inject error into current transport.
	// We need a handle to the current mock transport. Let's restructure.

	// Fire the stale grace timer from cycle 1.
	fc.Fire(0)

	// Give hub time to process the stale grace message.
	time.Sleep(50 * time.Millisecond)

	// OnDisconnect should NOT have fired — the timer is stale (epoch mismatch).
	select {
	case <-disconnected:
		require.Fail(t, "OnDisconnect fired — stale timer was not correctly ignored")
	default:
	}
}

// ── Transport callbacks: drop fires on suspend ──────────────────────────────

func TestComponent_OnTransportDrop_FiresOnSuspend(t *testing.T) {
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

func TestComponent_OnTransportRestore_FiresOnResume(t *testing.T) {
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

func TestComponent_TransportCallbacks_NotFired_WithoutResumeWindow(t *testing.T) {
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
