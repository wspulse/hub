package wspulse_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Spike tests — temporary file to verify coder/websocket error behavior.
// Findings inform isNormalClose implementation in Task 3.
// Delete this file after migration is complete.

func TestSpike_NormalClose_ReturnsCloseStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		// Read one message then close normally.
		_, _, _ = conn.Read(context.Background())
		_ = conn.Close(websocket.StatusNormalClosure, "bye")
	}))
	defer srv.Close()

	ctx := context.Background()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)

	// Send a message to trigger server-side close.
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte("hello")))

	// Read should return a close error with StatusNormalClosure.
	_, _, readErr := conn.Read(ctx)
	require.Error(t, readErr)

	code := websocket.CloseStatus(readErr)
	assert.Equal(t, websocket.StatusNormalClosure, code,
		"expected StatusNormalClosure, got error: %v", readErr)
}

func TestSpike_TCPDrop_ReturnsNetError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		// Wait for signal then abruptly close TCP (no close frame).
		_, _, _ = conn.Read(context.Background())
		_ = conn.CloseNow()
	}))
	defer srv.Close()

	ctx := context.Background()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)

	// Trigger server-side TCP drop.
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte("drop")))

	// Read should return a non-close error (CloseStatus == -1).
	_, _, readErr := conn.Read(ctx)
	require.Error(t, readErr)

	code := websocket.CloseStatus(readErr)
	assert.Equal(t, websocket.StatusCode(-1), code,
		"expected no close status (-1) for TCP drop, got code %d, error: %v", code, readErr)

	// Verify it is NOT context.Canceled.
	assert.False(t, errors.Is(readErr, context.Canceled),
		"TCP drop should not be context.Canceled")
}

func TestSpike_ContextCancel_ReturnsCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		// Keep connection alive — just block on read.
		_, _, _ = conn.Read(context.Background())
	}))
	defer srv.Close()

	ctx := context.Background()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()

	// Cancel context while Read is blocking.
	readCtx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, _, readErr := conn.Read(readCtx)
	require.Error(t, readErr)

	assert.True(t, errors.Is(readErr, context.Canceled),
		"expected context.Canceled, got: %v", readErr)

	code := websocket.CloseStatus(readErr)
	assert.Equal(t, websocket.StatusCode(-1), code,
		"context cancel should not produce a close status")
}

func TestSpike_CloseNow_OnRead_ReturnsNetError(t *testing.T) {
	// Simulates what happens when pingPump calls CloseNow while readPump is
	// blocking on Read. This is the ping-failure path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		// Keep connection alive.
		_, _, _ = conn.Read(context.Background())
	}))
	defer srv.Close()

	ctx := context.Background()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)

	// CloseNow from another goroutine while Read is blocking.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = conn.CloseNow()
	}()

	_, _, readErr := conn.Read(ctx)
	require.Error(t, readErr)

	// Key assertion: CloseNow-induced error is NOT context.Canceled.
	assert.False(t, errors.Is(readErr, context.Canceled),
		"CloseNow should not produce context.Canceled, got: %v", readErr)

	// Verify it is distinguishable from normal close.
	code := websocket.CloseStatus(readErr)
	isNetErr := errors.Is(readErr, net.ErrClosed) || code == -1
	assert.True(t, isNetErr,
		"expected net error or no close status, got code %d, error: %v", code, readErr)
}
