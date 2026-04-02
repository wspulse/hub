package wspulse_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	wspulse "github.com/wspulse/server"
)

func TestNewTestServer_ReturnsWSURL(t *testing.T) {
	url := wspulse.NewTestServer(t, func(r *http.Request) (string, string, error) {
		return "room", "", nil
	})
	require.True(t, strings.HasPrefix(url, "ws://"),
		"expected ws:// prefix, got %q", url)

	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
}
