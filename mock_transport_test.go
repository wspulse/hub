package wspulse_test

import (
	"net"
	"sync"
	"time"
)

// mockTransport is a channel-based, deterministic Transport implementation
// for component tests. Zero network I/O — all read/write operations use
// Go channels, giving the test full control over timing and data flow.
type mockTransport struct {
	readCh    chan readResult // test → readPump: inject messages or errors
	writeCh   chan writeCall  // writePump → test: capture outbound frames
	closeCh   chan struct{}   // closed once on Close()
	closeOnce sync.Once

	mu          sync.Mutex
	readLimit   int64
	pongHandler func(string) error
	closed      bool
}

type readResult struct {
	messageType int
	data        []byte
	err         error
}

type writeCall struct {
	messageType int
	data        []byte
}

func newMockTransport() *mockTransport {
	return &mockTransport{
		readCh:  make(chan readResult, 16),
		writeCh: make(chan writeCall, 256),
		closeCh: make(chan struct{}),
	}
}

func (m *mockTransport) ReadMessage() (int, []byte, error) {
	select {
	case r := <-m.readCh:
		return r.messageType, r.data, r.err
	case <-m.closeCh:
		return 0, nil, &net.OpError{Op: "read", Err: net.ErrClosed}
	}
}

func (m *mockTransport) WriteMessage(messageType int, data []byte) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return &net.OpError{Op: "write", Err: net.ErrClosed}
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

func (m *mockTransport) SetReadLimit(limit int64) {
	m.mu.Lock()
	m.readLimit = limit
	m.mu.Unlock()
}

func (m *mockTransport) SetReadDeadline(_ time.Time) error { return nil }
func (m *mockTransport) SetWriteDeadline(_ time.Time) error { return nil }

func (m *mockTransport) SetPongHandler(h func(string) error) {
	m.mu.Lock()
	m.pongHandler = h
	m.mu.Unlock()
}

func (m *mockTransport) Close() error {
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
func (m *mockTransport) InjectMessage(messageType int, data []byte) {
	m.readCh <- readResult{messageType: messageType, data: data}
}

// InjectError simulates a read error (e.g. connection drop).
func (m *mockTransport) InjectError(err error) {
	m.readCh <- readResult{err: err}
}

// SimulatePong triggers the pong handler as if a pong frame arrived.
func (m *mockTransport) SimulatePong() error {
	m.mu.Lock()
	h := m.pongHandler
	m.mu.Unlock()
	if h != nil {
		return h("")
	}
	return nil
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
