package wspulse

// Clock exports the internal clock interface for testing only.
type Clock = clock

// WithClock returns a ServerOption that sets the clock. Test-only —
// this file is only compiled during test builds.
func WithClock(c Clock) ServerOption {
	return func(cfg *serverConfig) { cfg.clock = c }
}
