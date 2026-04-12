package wstest_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	c, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.CloseNow() })
}
