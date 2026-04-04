package local

import "sync"

// RingBuffer is a fixed-size circular buffer that stores the most recent bytes
// written to it. When full, old data is silently overwritten.
type RingBuffer struct {
	mu   sync.Mutex
	buf  []byte
	size int
	pos  int // next write position
	full bool
}

// NewRingBuffer creates a ring buffer with the given capacity in bytes.
func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{
		buf:  make([]byte, size),
		size: size,
	}
}

// Write appends data to the ring buffer, overwriting the oldest data if necessary.
func (rb *RingBuffer) Write(data []byte) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	for len(data) > 0 {
		n := copy(rb.buf[rb.pos:], data)
		data = data[n:]
		rb.pos += n
		if rb.pos >= rb.size {
			rb.pos = 0
			rb.full = true
		}
	}
}

// Bytes returns a copy of all buffered data in chronological order.
func (rb *RingBuffer) Bytes() []byte {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if !rb.full {
		out := make([]byte, rb.pos)
		copy(out, rb.buf[:rb.pos])
		return out
	}

	out := make([]byte, rb.size)
	// Data from pos..end is older, 0..pos is newer
	n := copy(out, rb.buf[rb.pos:])
	copy(out[n:], rb.buf[:rb.pos])
	return out
}

// Len returns the number of bytes currently stored.
func (rb *RingBuffer) Len() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if rb.full {
		return rb.size
	}
	return rb.pos
}
