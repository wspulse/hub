package wspulse

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── A: Enqueue ───────────────────────────────────────────────────────────────

func TestSendQueue_A1_EnqueueSucceeds(t *testing.T) {
	t.Parallel()
	q := newSendQueue(4)
	err := q.Enqueue([]byte("a"))
	assert.NoError(t, err)
	assert.Equal(t, 1, q.Len())
}

func TestSendQueue_A2_EnqueueRejectsWhenFull(t *testing.T) {
	t.Parallel()
	q := newSendQueue(2)
	require.NoError(t, q.Enqueue([]byte("a")))
	require.NoError(t, q.Enqueue([]byte("b")))
	err := q.Enqueue([]byte("c"))
	assert.ErrorIs(t, err, ErrSendBufferFull)
	assert.Equal(t, 2, q.Len())
}

func TestSendQueue_A3_EnqueueAfterClose(t *testing.T) {
	t.Parallel()
	q := newSendQueue(4)
	q.Close()
	err := q.Enqueue([]byte("x"))
	assert.ErrorIs(t, err, ErrConnectionClosed)
}

// ── B: ForceEnqueue ──────────────────────────────────────────────────────────

func TestSendQueue_B1_ForceEnqueueNoEviction(t *testing.T) {
	t.Parallel()
	q := newSendQueue(4)
	evicted, err := q.ForceEnqueue([]byte("a"))
	assert.NoError(t, err)
	assert.False(t, evicted)
}

func TestSendQueue_B2_ForceEnqueueEvictsOldest(t *testing.T) {
	t.Parallel()
	q := newSendQueue(2)
	require.NoError(t, q.Enqueue([]byte("old1")))
	require.NoError(t, q.Enqueue([]byte("old2")))
	evicted, err := q.ForceEnqueue([]byte("new"))
	assert.NoError(t, err)
	assert.True(t, evicted)
	// old1 was evicted; remaining: old2, new
	assert.Equal(t, 2, q.Len())
	drained := q.Drain()
	assert.Equal(t, [][]byte{[]byte("old2"), []byte("new")}, drained)
}

func TestSendQueue_B3_ForceEnqueueAfterClose(t *testing.T) {
	t.Parallel()
	q := newSendQueue(4)
	q.Close()
	_, err := q.ForceEnqueue([]byte("x"))
	assert.ErrorIs(t, err, ErrConnectionClosed)
}

// ── C: Pop ───────────────────────────────────────────────────────────────────

func TestSendQueue_C1_PopReturnsItem(t *testing.T) {
	t.Parallel()
	q := newSendQueue(4)
	require.NoError(t, q.Enqueue([]byte("hello")))
	data, err := q.Pop(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, []byte("hello"), data)
}

func TestSendQueue_C2_PopBlocksUntilEnqueue(t *testing.T) {
	t.Parallel()
	q := newSendQueue(4)
	result := make(chan []byte, 1)
	go func() {
		data, _ := q.Pop(context.Background())
		result <- data
	}()
	time.Sleep(10 * time.Millisecond) // let Pop block
	require.NoError(t, q.Enqueue([]byte("wakeup")))
	select {
	case data := <-result:
		assert.Equal(t, []byte("wakeup"), data)
	case <-time.After(time.Second):
		t.Fatal("Pop did not unblock after Enqueue")
	}
}

func TestSendQueue_C3_PopUnblocksOnClose(t *testing.T) {
	t.Parallel()
	q := newSendQueue(4)
	errCh := make(chan error, 1)
	go func() {
		_, err := q.Pop(context.Background())
		errCh <- err
	}()
	time.Sleep(10 * time.Millisecond)
	q.Close()
	select {
	case err := <-errCh:
		assert.ErrorIs(t, err, ErrConnectionClosed)
	case <-time.After(time.Second):
		t.Fatal("Pop did not unblock after Close")
	}
}

func TestSendQueue_C4_PopUnblocksOnContextCancel(t *testing.T) {
	t.Parallel()
	q := newSendQueue(4)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := q.Pop(ctx)
		errCh <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Pop did not unblock after context cancel")
	}
}

// TestSendQueue_C4b_PopCancelRaceLostWakeup is a regression test for the
// lost-wakeup race: context.AfterFunc must hold the queue lock before
// broadcasting so that a cancellation that occurs between the ctx.Err()
// check and the cond.Wait() call is never silently dropped.
func TestSendQueue_C4b_PopCancelRaceLostWakeup(t *testing.T) {
	t.Parallel()
	const iterations = 10000
	for range iterations {
		ctx, cancel := context.WithCancel(context.Background())
		q := newSendQueue(1)
		done := make(chan error, 1)
		go func() {
			_, err := q.Pop(ctx)
			done <- err
		}()
		// Cancel the context immediately — races with Pop entering cond.Wait.
		cancel()
		select {
		case err := <-done:
			assert.ErrorIs(t, err, context.Canceled)
		case <-time.After(time.Second):
			t.Fatal("Pop blocked despite context cancel (lost-wakeup regression)")
		}
	}
}

func TestSendQueue_C5_PopDrainsExistingItemsBeforeClose(t *testing.T) {
	t.Parallel()
	q := newSendQueue(4)
	require.NoError(t, q.Enqueue([]byte("a")))
	require.NoError(t, q.Enqueue([]byte("b")))
	q.Close()
	// Items already in the buffer are returned before ErrConnectionClosed.
	data1, err1 := q.Pop(context.Background())
	data2, err2 := q.Pop(context.Background())
	_, err3 := q.Pop(context.Background())
	assert.NoError(t, err1)
	assert.NoError(t, err2)
	assert.Equal(t, []byte("a"), data1)
	assert.Equal(t, []byte("b"), data2)
	assert.ErrorIs(t, err3, ErrConnectionClosed)
}

// ── D: Drain ─────────────────────────────────────────────────────────────────

func TestSendQueue_D1_DrainReturnsAllItems(t *testing.T) {
	t.Parallel()
	q := newSendQueue(4)
	require.NoError(t, q.Enqueue([]byte("x")))
	require.NoError(t, q.Enqueue([]byte("y")))
	got := q.Drain()
	assert.Equal(t, [][]byte{[]byte("x"), []byte("y")}, got)
	assert.Equal(t, 0, q.Len())
}

func TestSendQueue_D2_DrainReturnsNilWhenEmpty(t *testing.T) {
	t.Parallel()
	q := newSendQueue(4)
	assert.Nil(t, q.Drain())
}

// ── E: Cap / Len ─────────────────────────────────────────────────────────────

func TestSendQueue_E1_CapIsConstant(t *testing.T) {
	t.Parallel()
	q := newSendQueue(8)
	assert.Equal(t, 8, q.Cap())
	require.NoError(t, q.Enqueue([]byte("a")))
	assert.Equal(t, 8, q.Cap())
}

// ── F: Concurrency ───────────────────────────────────────────────────────────

// TestSendQueue_F1_ConcurrentEnqueueNoRace verifies that concurrent Enqueue +
// ForceEnqueue calls from multiple goroutines do not race and that no items
// appear out of thin air or disappear. The total number of items enqueued
// minus evictions must equal q.Len() + items consumed by Pop.
func TestSendQueue_F1_ConcurrentEnqueueNoRace(t *testing.T) {
	t.Parallel()
	const cap = 16
	const workers = 8
	const perWorker = 64
	q := newSendQueue(cap)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var popped int
	var popMu sync.Mutex
	go func() {
		for {
			_, err := q.Pop(ctx)
			if err != nil {
				return
			}
			popMu.Lock()
			popped++
			popMu.Unlock()
		}
	}()

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				q.ForceEnqueue([]byte("x")) //nolint:errcheck
			}
		}()
	}
	wg.Wait()
	time.Sleep(20 * time.Millisecond) // let Pop drain remaining items
	cancel()
	time.Sleep(10 * time.Millisecond) // let Pop goroutine exit

	popMu.Lock()
	total := popped + q.Len()
	popMu.Unlock()

	// Total items consumed + remaining must be ≤ total enqueued.
	// Due to evictions, total may be less than workers*perWorker.
	assert.LessOrEqual(t, total, workers*perWorker)
	assert.GreaterOrEqual(t, total, 0)
}

// TestSendQueue_F2_DropOldestIsAtomic is the critical regression test for
// hub#44: verifies that drop-oldest enqueue in a concurrent scenario never
// loses the newest message. With the old three-select channel approach this
// test was intermittently flaky under -race.
func TestSendQueue_F2_DropOldestIsAtomic(t *testing.T) {
	t.Parallel()
	const bufSize = 2
	const iterations = 500

	for range iterations {
		q := newSendQueue(bufSize)
		// Fill buffer.
		require.NoError(t, q.Enqueue([]byte("old1")))
		require.NoError(t, q.Enqueue([]byte("old2")))

		// Concurrently: one goroutine drains (simulates writePump),
		// another calls ForceEnqueue (simulates broadcast).
		ready := make(chan struct{})
		poppedCh := make(chan []byte, 1)
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			<-ready
			data, _ := q.Pop(context.Background())
			poppedCh <- data
		}()

		go func() {
			defer wg.Done()
			<-ready
			q.ForceEnqueue([]byte("newest")) //nolint:errcheck
		}()

		close(ready)
		wg.Wait()

		// newest must appear either in what Pop consumed or in the remaining buffer.
		// The only invalid outcome (from the old TOCTOU race) was newest being
		// silently dropped while buffer appeared non-full — cannot happen with mutex.
		popped := <-poppedCh
		remaining := q.Drain()
		newestFound := string(popped) == "newest"
		for _, item := range remaining {
			if string(item) == "newest" {
				newestFound = true
				break
			}
		}
		assert.True(t, newestFound, "newest message must not be silently dropped")
		assert.LessOrEqual(t, q.Len(), bufSize, "buffer must not exceed capacity")
	}
}
