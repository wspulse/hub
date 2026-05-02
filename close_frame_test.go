package wspulse_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	core "github.com/wspulse/core"
	wspulse "github.com/wspulse/hub"
)

// ── Hub shutdown close frame ────────────────────────────────────────────────

// TestHub_Close_EmitsServerShuttingDown verifies that when the hub shuts down,
// each session's writePump emits a close frame with StatusGoingAway (1001) and
// the reason "server shutting down" — distinct from the default
// (StatusNormalClosure, "") used for application-initiated session close.
func TestHub_Close_EmitsServerShuttingDown(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	srv := wspulse.NewHub(
		acceptAll,
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
	)

	mt := injectAndWait(t, srv, "conn-1", "room-1", connected)

	// hub.Close() drives the heart shutdown path, which should close every
	// session with the (StatusGoingAway, "server shutting down") frame.
	srv.Close()

	// writePump's graceful close-frame send happens before the deferred
	// CloseNow that fires closeCh.
	<-mt.closeCh

	calls := mt.CloseCalls()
	require.Len(t, calls, 1, "expected exactly one Close(code, reason) call from writePump shutdown path")
	assert.Equal(t, core.StatusGoingAway, calls[0].code)
	assert.Equal(t, "server shutting down", calls[0].reason)
}

// ── Kick close frame ────────────────────────────────────────────────────────

// TestHub_Kick_EmitsKickedReason verifies that Hub.Kick produces a close
// frame with StatusNormalClosure (1000) and the reason "kicked", letting
// clients distinguish a server-initiated kick from a routine close on the
// wire.
func TestHub_Kick_EmitsKickedReason(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	srv := wspulse.NewHub(
		acceptAll,
		wspulse.WithOnConnect(func(_ wspulse.Connection) {
			connected <- struct{}{}
		}),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) {
			disconnected <- struct{}{}
		}),
	)
	t.Cleanup(srv.Close)

	mt := injectAndWait(t, srv, "kick-target", "room-1", connected)

	require.NoError(t, srv.Kick("kick-target"))
	requireReceive(t, disconnected)
	<-mt.closeCh

	calls := mt.CloseCalls()
	require.Len(t, calls, 1, "expected exactly one Close(code, reason) call from kick path")
	assert.Equal(t, core.StatusNormalClosure, calls[0].code)
	assert.Equal(t, "kicked", calls[0].reason)
}
