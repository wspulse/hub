package wspulse_test

import (
	"testing"
)

// requireReceive blocks until a value arrives on ch. If production code has a
// bug and the channel never signals, `go test -timeout` (default 10 min) kills
// the binary with a full goroutine stack dump — far more useful than a short
// time.After that becomes a flaky-test source under -race -count=N.
func requireReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	return <-ch
}
