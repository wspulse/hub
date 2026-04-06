package wspulse

// ringBuffer is a fixed-capacity circular buffer of raw encoded frames.
// Used by session to buffer outbound frames while the WebSocket transport
// is disconnected (during the resume window).
//
// A channel is not suitable here because the resume buffer requires two
// semantics that channels cannot provide:
//   - Head-drop: when full, the oldest frame is silently overwritten to
//     make room for the newest — channels can only block or fail.
//   - Batch drain: on reconnect, all buffered frames must be read out in
//     FIFO order as a single slice for replay — channels only support
//     one-at-a-time receive with no "drain all" primitive.
//
// Not safe for concurrent use — callers must hold a lock.
type ringBuffer struct {
	data [][]byte
	head int // index of the oldest element
	size int // number of elements currently stored
	cap  int // maximum capacity
}

// newRingBuffer creates a ring buffer with the given capacity.
// cap must be at least 1.
func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{
		data: make([][]byte, capacity),
		cap:  capacity,
	}
}

// Push appends data to the buffer. If the buffer is full, the oldest
// element is dropped (drop-oldest backpressure, matching the broadcast
// strategy used for the send channel). Returns true if an element was
// dropped to make room.
func (rb *ringBuffer) Push(data []byte) (dropped bool) {
	if rb.size < rb.cap {
		index := (rb.head + rb.size) % rb.cap
		rb.data[index] = data
		rb.size++
		return false
	}
	rb.data[rb.head] = data
	rb.head = (rb.head + 1) % rb.cap
	return true
}

// Drain returns all buffered frames in FIFO order and resets the buffer.
// Returns nil if the buffer is empty.
func (rb *ringBuffer) Drain() [][]byte {
	if rb.size == 0 {
		return nil
	}
	out := make([][]byte, rb.size)
	for i := 0; i < rb.size; i++ {
		index := (rb.head + i) % rb.cap
		out[i] = rb.data[index]
		rb.data[index] = nil
	}
	rb.head = 0
	rb.size = 0
	return out
}

// Len returns the number of frames currently buffered.
func (rb *ringBuffer) Len() int {
	return rb.size
}
