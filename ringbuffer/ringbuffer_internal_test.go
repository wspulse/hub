// Package ringbuffer — internal tests that require access to unexported fields.
package ringbuffer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPop_ZeroesSlotForGC verifies that Pop sets the vacated backing-array slot
// to the zero value of T. Without this, the array would retain a stale pointer
// and prevent the pointed-to value from being garbage collected.
func TestPop_ZeroesSlotForGC(t *testing.T) {
	t.Parallel()
	rb := New[*int](2)
	val := 42
	rb.Push(&val)
	rb.Pop()
	// rb.head is now 1 after the pop; the vacated slot is at index 0.
	assert.Nil(t, rb.data[0], "Pop must zero the vacated slot to avoid GC leaks")
}

// TestDrain_ZeroesSlotForGC verifies that Drain sets every drained slot to the
// zero value of T.
func TestDrain_ZeroesSlotForGC(t *testing.T) {
	t.Parallel()
	rb := New[*int](3)
	a, b := 1, 2
	rb.Push(&a)
	rb.Push(&b)
	rb.Drain()
	assert.Nil(t, rb.data[0], "Drain must zero slot 0")
	assert.Nil(t, rb.data[1], "Drain must zero slot 1")
}

// TestClear_ZeroesSlotForGC verifies that Clear zeros only the occupied slots,
// including when head is non-zero (wrapping case).
func TestClear_ZeroesSlotForGC(t *testing.T) {
	t.Parallel()
	rb := New[*int](3)
	a, b, c := 1, 2, 3
	rb.Push(&a)
	rb.Push(&b)
	rb.Push(&c)
	rb.Pop() // head advances to 1; slot 0 already zeroed by Pop
	// Now head=1, size=2, occupied slots are 1 and 2.
	rb.Clear()
	assert.Nil(t, rb.data[1], "Clear must zero slot 1")
	assert.Nil(t, rb.data[2], "Clear must zero slot 2")
}
