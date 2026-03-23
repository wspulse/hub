package wspulse

import "time"

// clock abstracts time operations for testability.
type clock interface {
	AfterFunc(d time.Duration, f func()) *time.Timer
	NewTicker(d time.Duration) *time.Ticker
}

// realClock delegates to the time package.
type realClock struct{}

func (realClock) AfterFunc(d time.Duration, f func()) *time.Timer {
	return time.AfterFunc(d, f)
}

func (realClock) NewTicker(d time.Duration) *time.Ticker {
	return time.NewTicker(d)
}
