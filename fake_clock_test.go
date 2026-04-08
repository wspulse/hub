package wspulse_test

import (
	"fmt"
	"sync"
	"time"
)

// fakeClock replaces both AfterFunc (grace timer) and NewTicker (heartbeat)
// with controllable fakes. Timers created by AfterFunc are stopped on
// creation and will not fire on their own — tests drive time explicitly
// via Fire(i). However, production code may call Reset(0) on the returned
// *time.Timer, which re-invokes the callback through the Go runtime
// (see AfterFunc GoDoc below for details).
type fakeClock struct {
	mu      sync.Mutex
	timers  []*fakeTimer
	tickers []*fakeTicker
}

type fakeTimer struct {
	d     time.Duration
	fn    func()
	timer *time.Timer
}

type fakeTicker struct {
	d      time.Duration
	ticker *time.Ticker
}

func newFakeClock() *fakeClock { return &fakeClock{} }

// AfterFunc returns a stopped *time.Timer created via time.AfterFunc.
// The timer will not fire on its own — tests must call fc.Fire(i) to
// invoke the callback. Because time.AfterFunc is used (not time.NewTimer),
// Reset(0) correctly re-invokes the callback, matching production semantics
// (e.g. session.Close() calls graceTimer.Reset(0) to force immediate expiry).
func (fc *fakeClock) AfterFunc(d time.Duration, f func()) *time.Timer {
	t := time.AfterFunc(time.Hour, f)
	t.Stop()
	fc.mu.Lock()
	fc.timers = append(fc.timers, &fakeTimer{d: d, fn: f, timer: t})
	fc.mu.Unlock()
	return t
}

// NewTicker returns a stopped ticker that will never fire on its own.
// This prevents heartbeat pings from interfering with tests that use
// fakeClock. The ticker is recorded for inspection if needed.
func (fc *fakeClock) NewTicker(d time.Duration) *time.Ticker {
	t := time.NewTicker(time.Hour)
	t.Stop()
	fc.mu.Lock()
	fc.tickers = append(fc.tickers, &fakeTicker{d: d, ticker: t})
	fc.mu.Unlock()
	return t
}

// Fire executes the callback of the i-th registered AfterFunc (0-based).
func (fc *fakeClock) Fire(i int) {
	fc.mu.Lock()
	if i >= len(fc.timers) {
		fc.mu.Unlock()
		panic(fmt.Sprintf("fakeClock.Fire(%d): only %d timers registered", i, len(fc.timers)))
	}
	ft := fc.timers[i]
	fc.mu.Unlock()
	ft.fn()
}

// Duration returns the duration passed to the i-th AfterFunc call.
func (fc *fakeClock) Duration(i int) time.Duration {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if i >= len(fc.timers) {
		panic(fmt.Sprintf("fakeClock.Duration(%d): only %d timers registered", i, len(fc.timers)))
	}
	return fc.timers[i].d
}

// TimerCount returns the number of registered AfterFunc timers.
func (fc *fakeClock) TimerCount() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return len(fc.timers)
}

// TickerCount returns the number of registered tickers.
func (fc *fakeClock) TickerCount() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return len(fc.tickers)
}

// Len returns the number of registered timers.
func (fc *fakeClock) Len() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return len(fc.timers)
}
