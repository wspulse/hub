// Package wstest provides test helpers for wspulse/hub.
//
// Import this package only in _test.go files to avoid pulling test
// infrastructure into production binaries.
package wstest

import (
	"net/http/httptest"
	"strings"
	"testing"

	wspulse "github.com/wspulse/hub"
)

// NewTestHub creates an in-process test server for use in application tests.
// It returns the WebSocket URL (ws://...) ready for client.Dial.
// The server and its HTTP listener are automatically cleaned up when the test ends.
func NewTestHub(t testing.TB, connect wspulse.ConnectFunc, opts ...wspulse.HubOption) string {
	t.Helper()
	srv := wspulse.NewHub(connect, opts...)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		srv.Close()
		ts.Close()
	})
	return "ws" + strings.TrimPrefix(ts.URL, "http")
}
