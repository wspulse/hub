package wspulse_test

import (
	"context"
	"net"
	"sync"
	"time"

	core "github.com/wspulse/core"
)

// mockTransport is a channel-based, deterministic Transport implementation
// for component tests. Zero network I/O — all read/write/ping operations use
// Go channels, giving the test full control over timing and data flow.
//
// Ping behavior: by default Ping returns nil immediately (healthy). Tests that
// need to control ping liveness call SetPingHandler to provide a custom
// function. This ensures tests never depend on real timeouts.
type mockTransport struct {
	readCh    chan readResult // test → readPump: inject messages or errors
	writeCh   chan writeCall  // writePump → test: capture outbound messages
	closeCh   chan struct{}   // closed once on Close() or CloseNow()
	closeOnce sync.Once

	mu          sync.Mutex
	readLimit   int64
	closed      bool
	pingHandler func(ctx context.Context) error // nil = always succeed
}

type readResult struct {
	messageType core.MessageType
	data        []byte
	err         error
}

type writeCall struct {
	messageType core.MessageType
	data        []byte
}

func newMockTransport() *mockTransport {
	return &mockTransport{
		readCh:  make(chan readResult, 16),
		writeCh: make(chan writeCall, 256),
		closeCh: make(chan struct{}),
	}
}

func (m *mockTransport) Read(ctx context.Context) (core.MessageType, []byte, error) {
	select {
	case r := <-m.readCh:
		return r.messageType, r.data, r.err
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-m.closeCh:
		return 0, nil, net.ErrClosed
	}
}

func (m *mockTransport) Write(ctx context.Context, messageType core.MessageType, data []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return net.ErrClosed
	}
	m.mu.Unlock()

	copied := make([]byte, len(data))
	copy(copied, data)
	select {
	case m.writeCh <- writeCall{messageType: messageType, data: copied}:
	default:
		// Drop if test isn't consuming — prevents writePump from blocking.
	}
	return nil
}

func (m *mockTransport) Ping(ctx context.Context) error {
	m.mu.Lock()
	handler := m.pingHandler
	m.mu.Unlock()

	if handler != nil {
		return handler(ctx)
	}
	// Default: healthy — return nil immediately.
	return nil
}

// SetPingHandler installs a custom Ping handler for tests that need to control
// ping liveness (e.g. simulating pong timeout). Pass nil to revert to the
// default (always succeed).
func (m *mockTransport) SetPingHandler(fn func(ctx context.Context) error) {
	m.mu.Lock()
	m.pingHandler = fn
	m.mu.Unlock()
}

func (m *mockTransport) SetReadLimit(limit int64) {
	m.mu.Lock()
	m.readLimit = limit
	m.mu.Unlock()
}

func (m *mockTransport) Close(_ core.StatusCode, _ string) error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		close(m.closeCh)
	})
	return nil
}

func (m *mockTransport) CloseNow() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		close(m.closeCh)
	})
	return nil
}

// ── Test helpers ─────────────────────────────────────────────────────────────

// InjectMessage simulates a message arriving from the peer.
func (m *mockTransport) InjectMessage(messageType core.MessageType, data []byte) {
	m.readCh <- readResult{messageType: messageType, data: data}
}

// InjectError simulates a read error (e.g. connection drop).
func (m *mockTransport) InjectError(err error) {
	m.readCh <- readResult{err: err}
}

// DrainWrites reads all pending writes from the write channel.
func (m *mockTransport) DrainWrites() []writeCall {
	var calls []writeCall
	for {
		select {
		case c := <-m.writeCh:
			calls = append(calls, c)
		default:
			return calls
		}
	}
}

// WaitWrite waits for a single write with timeout.
func (m *mockTransport) WaitWrite(timeout time.Duration) (writeCall, bool) {
	select {
	case c := <-m.writeCh:
		return c, true
	case <-time.After(timeout):
		return writeCall{}, false
	}
}
