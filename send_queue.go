package wspulse

import (
	"context"
	"sync"

	"github.com/wspulse/hub/ring"
)

// sendQueue is a concurrent, fixed-capacity FIFO queue for outbound messages.
// It wraps a ring.Buffer[[]byte] with a mutex and condition variable
// to provide a blocking Pop and two enqueue strategies:
//
//   - Enqueue: rejects when full (used by session.Send — caller should know)
//   - ForceEnqueue: evicts the oldest message when full (used by Broadcast)
//
// This eliminates the TOCTOU race present in the previous three-select
// drop-oldest pattern on session.send chan []byte.
//
// Goroutine ownership: any goroutine may call Enqueue/ForceEnqueue concurrently.
// Pop is intended to be called by a single writePump goroutine.
// Close must be called exactly once when the session terminates.
type sendQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    *ring.Buffer[[]byte]
	closed bool
}

func newSendQueue(capacity int) *sendQueue {
	q := &sendQueue{
		buf: ring.New[[]byte](capacity),
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Enqueue adds data to the queue.
// Returns ErrSendBufferFull if the queue is full.
// Returns ErrConnectionClosed if the queue has been closed.
func (q *sendQueue) Enqueue(data []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return ErrConnectionClosed
	}
	if !q.buf.Push(data) {
		return ErrSendBufferFull
	}
	q.cond.Signal()
	return nil
}

// ForceEnqueue adds data to the queue, evicting the oldest item if full.
// Returns true if an item was evicted.
// Returns ErrConnectionClosed if the queue has been closed.
func (q *sendQueue) ForceEnqueue(data []byte) (evicted bool, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return false, ErrConnectionClosed
	}
	evicted = q.buf.ForcePush(data)
	q.cond.Signal()
	return evicted, nil
}

// Pop blocks until an item is available, the context is cancelled, or the
// queue is closed. Returns ErrConnectionClosed when the queue is closed and
// empty. Returns context.Err() when the context is cancelled.
//
// Must be called by a single goroutine (writePump).
func (q *sendQueue) Pop(ctx context.Context) ([]byte, error) {
	// Register context cancellation to wake the blocked cond.Wait.
	// The callback must acquire q.mu before broadcasting to prevent
	// a lost-wakeup: if the context is cancelled between the ctx.Err()
	// check and the cond.Wait() call, the broadcast would fire with no
	// waiters. Acquiring the lock serializes the broadcast after Wait().
	stop := context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.cond.Broadcast()
		q.mu.Unlock()
	})
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()

	for {
		if item, ok := q.buf.Pop(); ok {
			return item, nil
		}
		if q.closed {
			return nil, ErrConnectionClosed
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		q.cond.Wait()
	}
}

// Drain removes and returns all items in FIFO order without blocking.
// Used by the resume transition goroutine to transfer buffered messages into
// the new session's outbound send queue before its writePump starts.
//
// Must not be called concurrently with Pop.
func (q *sendQueue) Drain() [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.buf.Drain()
}

// Len returns the number of items currently in the queue.
func (q *sendQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.buf.Len()
}

// Cap returns the maximum capacity of the queue.
func (q *sendQueue) Cap() int {
	return q.buf.Cap() // immutable after construction, no lock needed
}

// Close marks the queue as closed and wakes any goroutine blocked in Pop.
// Safe to call exactly once. Subsequent Enqueue/ForceEnqueue calls return
// ErrConnectionClosed.
func (q *sendQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}
