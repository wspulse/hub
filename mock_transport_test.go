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
type mockTransport struct {
	readCh    chan readResult // test → readPump: inject messages or errors
	writeCh   chan writeCall  // writePump → test: capture outbound frames
	pingErr   chan error      // test → pingPump: nil = pong ok; non-nil = simulate timeout
	closeCh   chan struct{}   // closed once on Close() or CloseNow()
	closeOnce sync.Once

	mu        sync.Mutex
	readLimit int64
	closed    bool
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
		pingErr: make(chan error, 16),
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

func (m *mockTransport) Write(_ context.Context, messageType core.MessageType, data []byte) error {
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
	select {
	case err := <-m.pingErr:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-m.closeCh:
		return net.ErrClosed
	}
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
