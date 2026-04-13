package ring_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wspulse/hub/ring"
)

// ── A: New ───────────────────────────────────────────────────────────────────

func TestNew_A1_ValidCapacity(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](4)
	require.NotNil(t, rb)
	assert.Equal(t, 0, rb.Len())
	assert.Equal(t, 4, rb.Cap())
}

func TestNew_A2_CapacityOne(t *testing.T) {
	t.Parallel()
	rb := ring.New[[]byte](1)
	assert.Equal(t, 1, rb.Cap())
}

func TestNew_A3_PanicsOnZeroCapacity(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { ring.New[int](0) })
}

func TestNew_A4_PanicsOnNegativeCapacity(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { ring.New[int](-1) })
}

// ── B: Push ───────────────────────────────────────────────────────────────────

func TestPush_B1_ReturnsTrueWhenNotFull(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](4)
	assert.True(t, rb.Push(1))
	assert.Equal(t, 1, rb.Len())
}

func TestPush_B2_ReturnsFalseWhenFull(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](2)
	rb.Push(1)
	rb.Push(2)
	assert.False(t, rb.Push(3))
	assert.Equal(t, 2, rb.Len())
}

func TestPush_B3_ItemNotAddedWhenFull(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](2)
	rb.Push(1)
	rb.Push(2)
	rb.Push(99) // should be rejected
	got := rb.Drain()
	assert.Equal(t, []int{1, 2}, got)
}

func TestPush_B4_FillsToCapacity(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](3)
	assert.True(t, rb.Push(1))
	assert.True(t, rb.Push(2))
	assert.True(t, rb.Push(3))
	assert.Equal(t, 3, rb.Len())
}

// ── C: ForcePush ─────────────────────────────────────────────────────────────

func TestForcePush_C1_NoEvictionWhenNotFull(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](4)
	evicted := rb.ForcePush(1)
	assert.False(t, evicted)
	assert.Equal(t, 1, rb.Len())
}

func TestForcePush_C2_EvictsOldestWhenFull(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](2)
	rb.Push(1)
	rb.Push(2)
	evicted := rb.ForcePush(3)
	assert.True(t, evicted)
	got := rb.Drain()
	assert.Equal(t, []int{2, 3}, got)
}

func TestForcePush_C3_MultipleEvictions(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](2)
	rb.Push(1)
	rb.Push(2)
	rb.ForcePush(3) // evicts 1
	rb.ForcePush(4) // evicts 2
	got := rb.Drain()
	assert.Equal(t, []int{3, 4}, got)
}

func TestForcePush_C4_SingleCapacityAlwaysEvicts(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](1)
	rb.Push(1)
	evicted := rb.ForcePush(2)
	assert.True(t, evicted)
	got := rb.Drain()
	assert.Equal(t, []int{2}, got)
}

// ── D: Pop ────────────────────────────────────────────────────────────────────

func TestPop_D1_ReturnsFalseOnEmpty(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](4)
	v, ok := rb.Pop()
	assert.False(t, ok)
	assert.Zero(t, v)
}

func TestPop_D2_RemovesOldestFirst(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](4)
	rb.Push(10)
	rb.Push(20)
	rb.Push(30)
	v, ok := rb.Pop()
	assert.True(t, ok)
	assert.Equal(t, 10, v)
	assert.Equal(t, 2, rb.Len())
}

// TestPop_D3_ZeroesSlotForGC is intentionally omitted from the external test
// package because it requires access to internal fields to be meaningful.
// See ringbuffer_internal_test.go for the authoritative GC-zeroing assertion.

func TestPop_D4_Wraparound(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](3)
	rb.Push(1)
	rb.Push(2)
	rb.Push(3)
	rb.Pop() // removes 1, head advances
	rb.Push(4)
	got := rb.Drain()
	assert.Equal(t, []int{2, 3, 4}, got)
}

// ── E: Peek ───────────────────────────────────────────────────────────────────

func TestPeek_E1_ReturnsFalseOnEmpty(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](4)
	v, ok := rb.Peek()
	assert.False(t, ok)
	assert.Zero(t, v)
}

func TestPeek_E2_ReturnsOldestWithoutRemoving(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](4)
	rb.Push(10)
	rb.Push(20)
	v, ok := rb.Peek()
	assert.True(t, ok)
	assert.Equal(t, 10, v)
	assert.Equal(t, 2, rb.Len()) // size unchanged
}

// ── F: Drain ─────────────────────────────────────────────────────────────────

func TestDrain_F1_ReturnsNilOnEmpty(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](4)
	assert.Nil(t, rb.Drain())
}

func TestDrain_F2_ReturnsFIFOOrder(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](4)
	rb.Push(1)
	rb.Push(2)
	rb.Push(3)
	got := rb.Drain()
	assert.Equal(t, []int{1, 2, 3}, got)
}

func TestDrain_F3_ClearsBuffer(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](4)
	rb.Push(1)
	rb.Drain()
	assert.Equal(t, 0, rb.Len())
	assert.Nil(t, rb.Drain())
}

func TestDrain_F4_WraparoundOrder(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](3)
	rb.Push(1)
	rb.Push(2)
	rb.Push(3)
	rb.Pop()   // head advances to index 1
	rb.Push(4) // wraps to index 0
	got := rb.Drain()
	assert.Equal(t, []int{2, 3, 4}, got)
}

func TestDrain_F5_NonZeroHeadBeforeDrain(t *testing.T) {
	t.Parallel()
	// Advance head to non-zero position before drain to exercise the wrap path.
	rb := ring.New[int](4)
	rb.Push(1)
	rb.Push(2)
	rb.Push(3)
	rb.Pop() // head = 1
	rb.Pop() // head = 2
	rb.Push(10)
	rb.Push(20)
	got := rb.Drain()
	assert.Equal(t, []int{3, 10, 20}, got)
}

// ── G: Clear ─────────────────────────────────────────────────────────────────

func TestClear_G1_EmptiesBuffer(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](4)
	rb.Push(1)
	rb.Push(2)
	rb.Clear()
	assert.Equal(t, 0, rb.Len())
}

func TestClear_G2_IsIdempotent(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](4)
	rb.Clear()
	rb.Clear()
	assert.Equal(t, 0, rb.Len())
}

func TestClear_G3_NonZeroHeadClearsCorrectSlots(t *testing.T) {
	t.Parallel()
	rb := ring.New[*int](3)
	a, b, c := 1, 2, 3
	rb.Push(&a)
	rb.Push(&b)
	rb.Push(&c)
	rb.Pop() // head advances to 1
	rb.Clear()
	// After clear, buffer can be reused correctly.
	d := 4
	assert.True(t, rb.Push(&d))
	out := rb.Drain()
	require.Len(t, out, 1)
	assert.Equal(t, &d, out[0])
}

// ── H: Len / Cap ─────────────────────────────────────────────────────────────

func TestLenCap_H1_LenTracksSize(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](4)
	assert.Equal(t, 0, rb.Len())
	rb.Push(1)
	assert.Equal(t, 1, rb.Len())
	rb.Push(2)
	assert.Equal(t, 2, rb.Len())
	rb.Pop()
	assert.Equal(t, 1, rb.Len())
}

func TestLenCap_H2_CapIsConstant(t *testing.T) {
	t.Parallel()
	rb := ring.New[int](7)
	for range 7 {
		rb.Push(0)
	}
	assert.Equal(t, 7, rb.Cap())
}

// ── Benchmarks ───────────────────────────────────────────────────────────────

func BenchmarkPush(b *testing.B) {
	// Pre-fill half the buffer so Push succeeds every iteration.
	rb := ring.New[[]byte](512)
	data := make([]byte, 64)
	for range 256 {
		rb.Push(data)
	}
	b.ResetTimer()
	for range b.N {
		if !rb.Push(data) {
			rb.Pop()
		}
	}
}

func BenchmarkForcePush(b *testing.B) {
	rb := ring.New[[]byte](256)
	data := make([]byte, 64)
	b.ResetTimer()
	for range b.N {
		rb.ForcePush(data)
	}
}

func BenchmarkPop(b *testing.B) {
	rb := ring.New[[]byte](256)
	data := make([]byte, 64)
	// Pre-fill.
	for range 256 {
		rb.Push(data)
	}
	b.ResetTimer()
	for range b.N {
		if _, ok := rb.Pop(); !ok {
			rb.ForcePush(data)
		}
	}
}

func BenchmarkDrain(b *testing.B) {
	rb := ring.New[[]byte](256)
	data := make([]byte, 64)
	b.ResetTimer()
	for range b.N {
		for range 256 {
			rb.ForcePush(data)
		}
		rb.Drain()
	}
}
