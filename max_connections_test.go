package wspulse_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	wspulse "github.com/wspulse/server"
)

// ── WithMaxConnections option validation ────────────────────────────────────

func TestWithMaxConnections_NegativePanics(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { wspulse.WithMaxConnections(-1) })
}

func TestWithMaxConnections_ZeroAccepted(t *testing.T) {
	t.Parallel()
	assert.NotPanics(t, func() { wspulse.WithMaxConnections(0) })
}

func TestWithMaxConnections_PositiveAccepted(t *testing.T) {
	t.Parallel()
	assert.NotPanics(t, func() { wspulse.WithMaxConnections(100) })
}

// ── Zero means unlimited ────────────────────────────────────────────────────

func TestMaxConnections_ZeroMeansUnlimited(t *testing.T) {
	t.Parallel()
	const n = 5
	connected := make(chan struct{}, n)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithMaxConnections(0),
		wspulse.WithOnConnect(func(_ wspulse.Connection) { connected <- struct{}{} }),
	)
	t.Cleanup(srv.Close)

	for i := 0; i < n; i++ {
		injectAndWait(t, srv, fmt.Sprintf("conn-%d", i), "room", connected)
	}
	assert.Equal(t, int64(n), wspulse.ActiveConnections(srv))
}

// ── Rejects over limit ──────────────────────────────────────────────────────

func TestMaxConnections_RejectsOverLimit(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithMaxConnections(1),
		wspulse.WithOnConnect(func(_ wspulse.Connection) { connected <- struct{}{} }),
	)
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// Fill the single slot via InjectTransport.
	injectAndWait(t, srv, "conn-1", "room", connected)
	require.Equal(t, int64(1), wspulse.ActiveConnections(srv))

	// Second connection via HTTP — expect 503.
	resp, err := http.Get(ts.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// ── Allows after disconnect ─────────────────────────────────────────────────

func TestMaxConnections_AllowsAfterDisconnect(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 2)
	disconnected := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithMaxConnections(1),
		wspulse.WithOnConnect(func(_ wspulse.Connection) { connected <- struct{}{} }),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) { disconnected <- struct{}{} }),
	)
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	mt := injectAndWait(t, srv, "conn-1", "room", connected)
	require.Equal(t, int64(1), wspulse.ActiveConnections(srv))

	// Disconnect.
	mt.InjectError(errors.New("connection closed"))
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnDisconnect")
	}
	require.Equal(t, int64(0), wspulse.ActiveConnections(srv))

	// New HTTP request — should NOT get 503.
	resp, err := http.Get(ts.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.NotEqual(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// ── Resume bypasses cap ─────────────────────────────────────────────────────

func TestMaxConnections_ResumeBypassesCap(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 2)
	dropped := make(chan struct{}, 1)
	restored := make(chan struct{}, 1)
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithMaxConnections(1),
		wspulse.WithResumeWindow(5*time.Second),
		wspulse.WithOnConnect(func(_ wspulse.Connection) { connected <- struct{}{} }),
		wspulse.WithOnTransportDrop(func(_ wspulse.Connection, _ error) { dropped <- struct{}{} }),
		wspulse.WithOnTransportRestore(func(_ wspulse.Connection) { restored <- struct{}{} }),
	)
	t.Cleanup(srv.Close)

	// Connect and drop → suspended (counter decremented to 0).
	connectAndDrop(t, srv, "conn-1", "room", connected, dropped)
	require.Equal(t, int64(0), wspulse.ActiveConnections(srv))

	// Resume with same connectionID → counter back to 1.
	reconnect(t, srv, "conn-1", "room", restored)
	assert.Equal(t, int64(1), wspulse.ActiveConnections(srv))
}

// ── Grace expired frees slot ────────────────────────────────────────────────

func TestMaxConnections_GraceExpiredFreesSlot(t *testing.T) {
	t.Parallel()
	connected := make(chan struct{}, 2)
	dropped := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	fc := newFakeClock()
	srv := wspulse.NewServer(
		acceptAll,
		wspulse.WithMaxConnections(1),
		wspulse.WithResumeWindow(30*time.Second),
		wspulse.WithClock(fc),
		wspulse.WithOnConnect(func(_ wspulse.Connection) { connected <- struct{}{} }),
		wspulse.WithOnTransportDrop(func(_ wspulse.Connection, _ error) { dropped <- struct{}{} }),
		wspulse.WithOnDisconnect(func(_ wspulse.Connection, _ error) { disconnected <- struct{}{} }),
	)
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// Connect and drop → suspended.
	connectAndDrop(t, srv, "conn-1", "room", connected, dropped)
	require.Equal(t, int64(0), wspulse.ActiveConnections(srv))

	// Fire the grace timer → session destroyed.
	fc.Fire(0)
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for OnDisconnect after grace expiry")
	}

	// New HTTP request — slot is free, should NOT get 503.
	resp, err := http.Get(ts.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.NotEqual(t, http.StatusServiceUnavailable, resp.StatusCode)
}
