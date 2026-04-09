package wspulse_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"go.uber.org/zap/zaptest"

	wspulse "github.com/wspulse/hub"
)

func acceptAll(r *http.Request) (roomID, connectionID string, err error) {
	return "test-room", "test-connection", nil
}

func TestHub_Send_ErrConnectionNotFound(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll)
	t.Cleanup(srv.Close)
	err := srv.Send("does-not-exist", wspulse.Frame{Event: "ping"})
	require.ErrorIs(t, err, wspulse.ErrConnectionNotFound)
}

func TestHub_Kick_ErrConnectionNotFound(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll)
	t.Cleanup(srv.Close)
	err := srv.Kick("does-not-exist")
	require.ErrorIs(t, err, wspulse.ErrConnectionNotFound)
}

func TestHub_GetConnections_UnknownRoom_ReturnsEmptySlice(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll)
	t.Cleanup(srv.Close)
	connections := srv.GetConnections("no-such-room")
	require.Empty(t, connections)
}

func TestHub_Close_SafeToCallTwice(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll)
	srv.Close() // first call
	srv.Close() // must not panic
}

func TestWithCodec_Nil_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = wspulse.WithCodec(nil)
	})
}

func TestWithHeartbeat_InvalidParams_Panics(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		ping, pong time.Duration
	}{
		{"ping == pong", 10 * time.Second, 10 * time.Second},
		{"ping > pong", 30 * time.Second, 10 * time.Second},
		{"ping zero", 0, 10 * time.Second},
		{"pong zero", 10 * time.Second, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Panics(t, func() {
				_ = wspulse.WithHeartbeat(tc.ping, tc.pong)
			}, "expected panic for pingPeriod=%v pongWait=%v", tc.ping, tc.pong)
		})
	}
}

func TestNewHub_NilConnect_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = wspulse.NewHub(nil)
	})
}

func TestWithLogger_Nil_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = wspulse.WithLogger(nil)
	})
}

func TestWithMaxMessageSize_Zero_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = wspulse.WithMaxMessageSize(0)
	})
}

// ── Option validation tests ───────────────────────────────────────────────────

func TestWithHeartbeat_ValidParams_Accepted(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll,
		wspulse.WithHeartbeat(5*time.Second, 15*time.Second),
	)
	t.Cleanup(srv.Close)
}

func TestWithHeartbeat_PingExceedsMax_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = wspulse.WithHeartbeat(6*time.Minute, 10*time.Minute)
	})
}

func TestWithHeartbeat_PongExceedsMax_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = wspulse.WithHeartbeat(1*time.Minute, 11*time.Minute)
	})
}

func TestWithWriteWait_ValidDuration_Accepted(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll,
		wspulse.WithWriteWait(5*time.Second),
	)
	t.Cleanup(srv.Close)
}

func TestWithWriteWait_Zero_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = wspulse.WithWriteWait(0)
	})
}

func TestWithWriteWait_ExceedsMax_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = wspulse.WithWriteWait(31 * time.Second)
	})
}

func TestWithMaxMessageSize_ValidSize_Accepted(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll,
		wspulse.WithMaxMessageSize(4096),
	)
	t.Cleanup(srv.Close)
}

func TestWithMaxMessageSize_ExceedsMax_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = wspulse.WithMaxMessageSize(64<<20 + 1)
	})
}

func TestWithSendBufferSize_Zero_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = wspulse.WithSendBufferSize(0)
	})
}

func TestWithSendBufferSize_ExceedsMax_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = wspulse.WithSendBufferSize(4097)
	})
}

func TestWithCheckOrigin_Nil_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = wspulse.WithCheckOrigin(nil)
	})
}

func TestWithResumeWindow_Negative_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = wspulse.WithResumeWindow(-time.Second)
	})
}

func TestWithResumeWindow_LargeValue_Accepted(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll,
		wspulse.WithResumeWindow(10*time.Minute),
	)
	srv.Close()
}

func TestWithCodec_ValidCodec_Accepted(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll,
		wspulse.WithCodec(wspulse.JSONCodec),
	)
	t.Cleanup(srv.Close)
}

func TestWithLogger_ValidLogger_Accepted(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll,
		wspulse.WithLogger(zaptest.NewLogger(t)),
	)
	t.Cleanup(srv.Close)
}

// ── Broadcast to empty or unknown room (already partially covered) ────────────

func TestHub_Broadcast_EmptyRoom_NoError(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll)
	t.Cleanup(srv.Close)
	err := srv.Broadcast("nonexistent-room", wspulse.Frame{Event: "msg"})
	require.NoError(t, err)
}

// ── Kick and Broadcast during server shutdown ─────────────────────────────────

func TestHub_Kick_AfterClose_ReturnsErrHubClosed(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll)
	srv.Close()
	require.ErrorIs(t, srv.Kick("any"), wspulse.ErrHubClosed)
}

// ── Send/Kick during hub close (both ErrHubClosed paths) ──────────────────

func TestHub_Send_AfterClose_ReturnsErrConnectionNotFound(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll)
	srv.Close()
	// After close, hub maps are empty — returns ErrConnectionNotFound.
	err := srv.Send("any", wspulse.Frame{Event: "x"})
	require.ErrorIs(t, err, wspulse.ErrConnectionNotFound)
}

// TestHub_Broadcast_AfterClose verifies Broadcast returns ErrHubClosed
// when called after the hub has been closed.
func TestHub_Broadcast_AfterClose(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll)
	srv.Close()
	err := srv.Broadcast("test-room", wspulse.Frame{Event: "hello"})
	require.ErrorIs(t, err, wspulse.ErrHubClosed)
}

// TestHub_Kick_AfterClose verifies Kick returns ErrHubClosed
// when called after the hub has been closed.
func TestHub_Kick_AfterClose(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll)
	srv.Close()
	err := srv.Kick("test-connection")
	require.ErrorIs(t, err, wspulse.ErrHubClosed)
}

func TestWithOnTransportDrop_AcceptsNil(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll,
		wspulse.WithOnTransportDrop(nil),
	)
	t.Cleanup(srv.Close)
}

func TestWithOnTransportRestore_AcceptsNil(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll,
		wspulse.WithOnTransportRestore(nil),
	)
	t.Cleanup(srv.Close)
}

func TestWithUpgraderBufferSize_Zero_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		wspulse.NewHub(acceptAll, wspulse.WithUpgraderBufferSize(0, 1024))
	})
}

func TestWithUpgraderBufferSize_NegativeWriteSize_Panics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		wspulse.NewHub(acceptAll, wspulse.WithUpgraderBufferSize(1024, -1))
	})
}

func TestWithUpgraderBufferSize_ValidSizes_Accepted(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(acceptAll, wspulse.WithUpgraderBufferSize(4096, 4096))
	t.Cleanup(srv.Close)
}

// TestHub_ConnectFunc_RejectBody_NoLeak verifies the HTTP 401 response
// body is a generic "unauthorized" string and does not leak the internal
// error from ConnectFunc. This unit test runs in the default make check
// gate (no integration tag required).
func TestHub_ConnectFunc_RejectBody_NoLeak(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(func(r *http.Request) (string, string, error) {
		return "", "", errors.New("internal: secret db details")
	})
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "unauthorized", strings.TrimSpace(string(body)),
		"internal error must not leak")
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
