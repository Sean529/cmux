package local

import (
	"bytes"
	"sync"
	"testing"
)

func TestRingBuffer_EmptyBuffer(t *testing.T) {
	rb := NewRingBuffer(64)
	got := rb.Bytes()
	if len(got) != 0 {
		t.Fatalf("expected empty slice from fresh buffer, got %d bytes: %q", len(got), got)
	}
	if rb.Len() != 0 {
		t.Fatalf("expected Len()=0 on fresh buffer, got %d", rb.Len())
	}
}

func TestRingBuffer_BasicWriteRead(t *testing.T) {
	rb := NewRingBuffer(64)
	data := []byte("hello world")
	rb.Write(data)

	got := rb.Bytes()
	if !bytes.Equal(got, data) {
		t.Fatalf("expected %q, got %q", data, got)
	}
	if rb.Len() != len(data) {
		t.Fatalf("expected Len()=%d, got %d", len(data), rb.Len())
	}
}

func TestRingBuffer_WrapAround(t *testing.T) {
	const size = 16
	rb := NewRingBuffer(size)

	// Write 24 bytes into a 16-byte buffer; first 8 bytes should be lost.
	data := []byte("AAAAAAAA" + "BBBBBBBB" + "CCCCCCCC")
	rb.Write(data)

	got := rb.Bytes()
	expected := data[len(data)-size:]
	if !bytes.Equal(got, expected) {
		t.Fatalf("expected last %d bytes %q, got %q", size, expected, got)
	}
	if rb.Len() != size {
		t.Fatalf("expected Len()=%d after wrap, got %d", size, rb.Len())
	}
}

func TestRingBuffer_ExactSizeWrite(t *testing.T) {
	const size = 32
	rb := NewRingBuffer(size)

	data := bytes.Repeat([]byte("X"), size)
	rb.Write(data)

	got := rb.Bytes()
	if !bytes.Equal(got, data) {
		t.Fatalf("expected %q, got %q", data, got)
	}
	if rb.Len() != size {
		t.Fatalf("expected Len()=%d, got %d", size, rb.Len())
	}
}

func TestRingBuffer_MultipleSmallWrites(t *testing.T) {
	const size = 16
	rb := NewRingBuffer(size)

	// Write 5 bytes at a time, 8 times = 40 bytes total into a 16-byte buffer.
	var allData []byte
	for i := 0; i < 8; i++ {
		chunk := bytes.Repeat([]byte{byte('A' + i)}, 5)
		rb.Write(chunk)
		allData = append(allData, chunk...)
	}

	got := rb.Bytes()
	expected := allData[len(allData)-size:]
	if !bytes.Equal(got, expected) {
		t.Fatalf("expected %q, got %q", expected, got)
	}
	if rb.Len() != size {
		t.Fatalf("expected Len()=%d, got %d", size, rb.Len())
	}
}

func TestRingBuffer_SingleByteWrites(t *testing.T) {
	const size = 8
	rb := NewRingBuffer(size)

	// Write 20 single bytes (0..19); last 8 should survive: 12..19
	for i := 0; i < 20; i++ {
		rb.Write([]byte{byte(i)})
	}

	got := rb.Bytes()
	if len(got) != size {
		t.Fatalf("expected %d bytes, got %d", size, len(got))
	}
	for i := 0; i < size; i++ {
		expected := byte(12 + i)
		if got[i] != expected {
			t.Fatalf("byte %d: expected %d, got %d", i, expected, got[i])
		}
	}
}

func TestRingBuffer_LargeSingleWrite(t *testing.T) {
	const size = 32
	rb := NewRingBuffer(size)

	// Write 3x buffer size in one call.
	data := make([]byte, size*3)
	for i := range data {
		data[i] = byte(i % 256)
	}
	rb.Write(data)

	got := rb.Bytes()
	expected := data[len(data)-size:]
	if !bytes.Equal(got, expected) {
		t.Fatalf("expected last %d bytes, got mismatch.\nexpected: %v\ngot:      %v", size, expected, got)
	}
	if rb.Len() != size {
		t.Fatalf("expected Len()=%d, got %d", size, rb.Len())
	}
}

func TestRingBuffer_LenBeforeAndAfterWrap(t *testing.T) {
	const size = 16
	rb := NewRingBuffer(size)

	// Before any writes
	if rb.Len() != 0 {
		t.Fatalf("expected Len()=0, got %d", rb.Len())
	}

	// Partial fill
	rb.Write([]byte("12345"))
	if rb.Len() != 5 {
		t.Fatalf("expected Len()=5, got %d", rb.Len())
	}

	// Fill to capacity
	rb.Write(bytes.Repeat([]byte("X"), 11))
	if rb.Len() != size {
		t.Fatalf("expected Len()=%d after filling, got %d", size, rb.Len())
	}

	// Overflow
	rb.Write([]byte("more data"))
	if rb.Len() != size {
		t.Fatalf("expected Len()=%d after overflow, got %d", size, rb.Len())
	}
}

func TestRingBuffer_ZeroLengthWrite(t *testing.T) {
	const size = 16
	rb := NewRingBuffer(size)

	// Write some data first.
	rb.Write([]byte("hello"))
	before := rb.Bytes()

	// Write empty slice.
	rb.Write([]byte{})
	after := rb.Bytes()

	if !bytes.Equal(before, after) {
		t.Fatalf("zero-length write changed buffer: before=%q after=%q", before, after)
	}
	if rb.Len() != 5 {
		t.Fatalf("expected Len()=5, got %d", rb.Len())
	}

	// Also test on fresh buffer.
	rb2 := NewRingBuffer(size)
	rb2.Write([]byte{})
	if rb2.Len() != 0 {
		t.Fatalf("expected Len()=0 after empty write on fresh buffer, got %d", rb2.Len())
	}
	if len(rb2.Bytes()) != 0 {
		t.Fatalf("expected empty Bytes() after empty write on fresh buffer")
	}
}

func TestRingBuffer_ConcurrentAccess(t *testing.T) {
	const size = 256
	const writers = 8
	const writesPerGoroutine = 1000

	rb := NewRingBuffer(size)
	var wg sync.WaitGroup

	// Spawn writers.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			chunk := bytes.Repeat([]byte{byte(id)}, 7)
			for i := 0; i < writesPerGoroutine; i++ {
				rb.Write(chunk)
			}
		}(w)
	}

	// Spawn concurrent readers.
	done := make(chan struct{})
	var readerWg sync.WaitGroup
	for r := 0; r < 4; r++ {
		readerWg.Add(1)
		go func() {
			defer readerWg.Done()
			for {
				select {
				case <-done:
					return
				default:
					got := rb.Bytes()
					l := rb.Len()
					// Len should never exceed buffer size.
					if l > size {
						t.Errorf("Len()=%d exceeds buffer size %d", l, size)
						return
					}
					// Bytes length should never exceed buffer size.
					if len(got) > size {
						t.Errorf("Bytes() returned %d bytes, exceeds buffer size %d", len(got), size)
						return
					}
				}
			}
		}()
	}

	wg.Wait()
	close(done)
	readerWg.Wait()

	// After all writes, buffer should be full.
	if rb.Len() != size {
		t.Fatalf("expected Len()=%d after stress test, got %d", size, rb.Len())
	}
	got := rb.Bytes()
	if len(got) != size {
		t.Fatalf("expected Bytes() to return %d bytes, got %d", size, len(got))
	}
}

func TestRingBuffer_BytesReturnsCopy(t *testing.T) {
	rb := NewRingBuffer(32)
	rb.Write([]byte("original"))

	got := rb.Bytes()
	// Mutate the returned slice.
	for i := range got {
		got[i] = 'Z'
	}

	// The buffer should be unaffected.
	after := rb.Bytes()
	if !bytes.Equal(after, []byte("original")) {
		t.Fatalf("Bytes() did not return a copy; buffer was mutated to %q", after)
	}
}

func TestRingBuffer_SequentialWrapVerification(t *testing.T) {
	// Verify exact ordering across multiple wrap-arounds.
	const size = 10
	rb := NewRingBuffer(size)

	// Write 7 bytes, then 8 bytes (total 15, wraps once).
	rb.Write([]byte("abcdefg"))      // pos=7, not full
	rb.Write([]byte("hijklmno"))     // wraps: first 3 fill pos 7-9, sets full, then 5 go to pos 0-4, pos=5

	got := rb.Bytes()
	// Total 15 bytes in 10-byte buffer: last 10 are "fghijklmno"
	expected := []byte("fghijklmno")
	if !bytes.Equal(got, expected) {
		t.Fatalf("expected %q, got %q", expected, got)
	}

	// Write 3 more bytes: "pqr"
	rb.Write([]byte("pqr"))
	got = rb.Bytes()
	// Previous state: buf contains wrapping data ending at pos=5.
	// After "pqr": pos moves 5->8, last 10 of all 18 bytes written = "ijklmnopqr"
	expected = []byte("ijklmnopqr")
	if !bytes.Equal(got, expected) {
		t.Fatalf("expected %q after second wrap, got %q", expected, got)
	}
}
