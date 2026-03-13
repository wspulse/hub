package wspulse

// ringBuffer is a fixed-capacity circular buffer of raw encoded frames.
// Used by session to buffer outbound frames while the WebSocket transport
// is disconnected (during the resume window).
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
// strategy used for the send channel).
func (rb *ringBuffer) Push(data []byte) {
	if rb.size < rb.cap {
		index := (rb.head + rb.size) % rb.cap
		rb.data[index] = data
		rb.size++
	} else {
		rb.data[rb.head] = data
		rb.head = (rb.head + 1) % rb.cap
	}
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
