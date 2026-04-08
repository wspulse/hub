package wstest_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/wspulse/server/wstest"
)

func TestNewTestServer_ReturnsWSURL(t *testing.T) {
	url := wstest.NewTestServer(t, func(r *http.Request) (string, string, error) {
		return "room", "", nil
	})
	require.True(t, strings.HasPrefix(url, "ws://"),
		"expected ws:// prefix, got %q", url)

	c, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if resp != nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
}
