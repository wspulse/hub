package wspulse

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// NewTestServer creates an in-process test server for use in application tests.
// It returns the WebSocket URL (ws://...) ready for client.Dial.
// The server and its HTTP listener are automatically cleaned up when the test ends.
func NewTestServer(t testing.TB, connect ConnectFunc, opts ...ServerOption) string {
	t.Helper()
	srv := NewServer(connect, opts...)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		srv.Close()
		ts.Close()
	})
	return "ws" + strings.TrimPrefix(ts.URL, "http")
}
