package wspulse_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	wspulse "github.com/wspulse/server"
)

func TestNewTestServer_ReturnsWSURL(t *testing.T) {
	url := wspulse.NewTestServer(t, func(r *http.Request) (string, string, error) {
		return "room", "", nil
	})
	if !strings.HasPrefix(url, "ws://") {
		t.Fatalf("expected ws:// prefix, got %q", url)
	}

	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("failed to dial %q: %v", url, err)
	}
	t.Cleanup(func() { _ = c.Close() })
}
