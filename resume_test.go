package wspulse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRingBuffer_PushAndDrain(t *testing.T) {
	t.Parallel()
	rb := newRingBuffer(4)

	rb.Push([]byte("a"))
	rb.Push([]byte("b"))
	rb.Push([]byte("c"))

	got := rb.Drain()
	require.Len(t, got, 3)
	assert.Equal(t, "a", string(got[0]))
	assert.Equal(t, "b", string(got[1]))
	assert.Equal(t, "c", string(got[2]))
}

func TestRingBuffer_DrainClearsBuffer(t *testing.T) {
	t.Parallel()
	rb := newRingBuffer(4)
	rb.Push([]byte("x"))
	rb.Drain()

	got := rb.Drain()
	require.Empty(t, got)
}

func TestRingBuffer_Wraparound(t *testing.T) {
	t.Parallel()
	rb := newRingBuffer(3)

	rb.Push([]byte("1"))
	rb.Push([]byte("2"))
	rb.Push([]byte("3"))
	rb.Push([]byte("4")) // overwrites "1"

	got := rb.Drain()
	require.Len(t, got, 3)
	assert.Equal(t, "2", string(got[0]))
	assert.Equal(t, "3", string(got[1]))
	assert.Equal(t, "4", string(got[2]))
}

func TestRingBuffer_Len(t *testing.T) {
	t.Parallel()
	rb := newRingBuffer(4)
	require.Equal(t, 0, rb.Len())
	rb.Push([]byte("a"))
	rb.Push([]byte("b"))
	require.Equal(t, 2, rb.Len())
}

func TestRingBuffer_SingleCapacity(t *testing.T) {
	t.Parallel()
	rb := newRingBuffer(1)
	rb.Push([]byte("first"))
	rb.Push([]byte("second"))

	got := rb.Drain()
	require.Len(t, got, 1)
	assert.Equal(t, "second", string(got[0]))
}

func TestRingBuffer_ExactCapacity(t *testing.T) {
	t.Parallel()
	rb := newRingBuffer(3)
	rb.Push([]byte("a"))
	rb.Push([]byte("b"))
	rb.Push([]byte("c"))

	got := rb.Drain()
	require.Len(t, got, 3)
	assert.Equal(t, "a", string(got[0]))
	assert.Equal(t, "b", string(got[1]))
	assert.Equal(t, "c", string(got[2]))
}

func TestRingBuffer_LenAfterWraparound(t *testing.T) {
	t.Parallel()
	rb := newRingBuffer(2)
	rb.Push([]byte("1"))
	rb.Push([]byte("2"))
	rb.Push([]byte("3")) // wraps

	require.Equal(t, 2, rb.Len())
}
