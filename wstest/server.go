// Package wstest provides test helpers for wspulse/server.
//
// Import this package only in _test.go files to avoid pulling test
// infrastructure into production binaries.
package wstest

import (
	"net/http/httptest"
	"strings"
	"testing"

	wspulse "github.com/wspulse/server"
)

// NewTestServer creates an in-process test server for use in application tests.
// It returns the WebSocket URL (ws://...) ready for client.Dial.
// The server and its HTTP listener are automatically cleaned up when the test ends.
func NewTestServer(t testing.TB, connect wspulse.ConnectFunc, opts ...wspulse.ServerOption) string {
	t.Helper()
	srv := wspulse.NewServer(connect, opts...)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		srv.Close()
		ts.Close()
	})
	return "ws" + strings.TrimPrefix(ts.URL, "http")
}
