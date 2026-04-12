package wstest_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/wspulse/hub/wstest"
)

func TestNewTestHub_ReturnsWSURL(t *testing.T) {
	url := wstest.NewTestHub(t, func(r *http.Request) (string, string, error) {
		return "room", "", nil
	})
	require.True(t, strings.HasPrefix(url, "ws://"),
		"expected ws:// prefix, got %q", url)

	c, _, err := websocket.Dial(context.Background(), url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.CloseNow() })
}
