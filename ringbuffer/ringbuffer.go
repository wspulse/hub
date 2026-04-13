// Package ringbuffer provides a generic fixed-capacity circular buffer.
// It is not safe for concurrent use — callers are responsible for
// synchronization when shared across goroutines.
package ringbuffer

// RingBuffer is a fixed-capacity FIFO circular buffer.
//
// The zero value is not usable; create instances with [New].
type RingBuffer[T any] struct {
	data []T
	head int // index of the oldest element
	size int // number of elements currently stored
	cap  int // maximum capacity
}

// New creates a RingBuffer with the given capacity.
//
// Panics if capacity < 1.
func New[T any](capacity int) *RingBuffer[T] {
	if capacity < 1 {
		panic("wspulse: ringbuffer: capacity must be at least 1")
	}
	return &RingBuffer[T]{
		data: make([]T, capacity),
		cap:  capacity,
	}
}

// Push appends item to the back of the buffer.
// Returns false if the buffer is full; the item is not added.
func (rb *RingBuffer[T]) Push(item T) bool {
	if rb.size == rb.cap {
		return false
	}
	rb.data[(rb.head+rb.size)%rb.cap] = item
	rb.size++
	return true
}

// ForcePush appends item to the back of the buffer. If the buffer is full,
// the oldest item is evicted to make room.
// Returns true if an item was evicted.
func (rb *RingBuffer[T]) ForcePush(item T) (evicted bool) {
	if rb.size == rb.cap {
		// Overwrite the oldest slot and advance head.
		rb.data[rb.head] = item
		rb.head = (rb.head + 1) % rb.cap
		return true
	}
	rb.data[(rb.head+rb.size)%rb.cap] = item
	rb.size++
	return false
}

// Pop removes and returns the oldest item.
// Returns the zero value of T and false if the buffer is empty.
func (rb *RingBuffer[T]) Pop() (T, bool) {
	if rb.size == 0 {
		var zero T
		return zero, false
	}
	item := rb.data[rb.head]
	var zero T
	rb.data[rb.head] = zero // release reference for GC
	rb.head = (rb.head + 1) % rb.cap
	rb.size--
	return item, true
}

// Peek returns the oldest item without removing it.
// Returns the zero value of T and false if the buffer is empty.
func (rb *RingBuffer[T]) Peek() (T, bool) {
	if rb.size == 0 {
		var zero T
		return zero, false
	}
	return rb.data[rb.head], true
}

// Drain removes and returns all items in FIFO order.
// Returns nil if the buffer is empty.
// After Drain, the buffer is empty and all internal slots are zeroed.
func (rb *RingBuffer[T]) Drain() []T {
	if rb.size == 0 {
		return nil
	}
	out := make([]T, rb.size)
	var zero T
	for i := range out {
		idx := (rb.head + i) % rb.cap
		out[i] = rb.data[idx]
		rb.data[idx] = zero // release reference for GC
	}
	rb.head = 0
	rb.size = 0
	return out
}

// Len returns the number of items currently in the buffer.
func (rb *RingBuffer[T]) Len() int {
	return rb.size
}

// Cap returns the maximum number of items the buffer can hold.
func (rb *RingBuffer[T]) Cap() int {
	return rb.cap
}

// Clear removes all items and releases slot references for GC.
func (rb *RingBuffer[T]) Clear() {
	var zero T
	for i := range rb.size {
		rb.data[(rb.head+i)%rb.cap] = zero
	}
	rb.head = 0
	rb.size = 0
}
