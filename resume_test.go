package wspulse

import (
	"testing"
)

func TestRingBuffer_PushAndDrain(t *testing.T) {
	rb := newRingBuffer(4)

	rb.Push([]byte("a"))
	rb.Push([]byte("b"))
	rb.Push([]byte("c"))

	got := rb.Drain()
	if len(got) != 3 {
		t.Fatalf("Drain: want 3 items, got %d", len(got))
	}
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if string(got[i]) != w {
			t.Errorf("Drain[%d]: want %q, got %q", i, w, string(got[i]))
		}
	}
}

func TestRingBuffer_DrainClearsBuffer(t *testing.T) {
	rb := newRingBuffer(4)
	rb.Push([]byte("x"))
	rb.Drain()

	got := rb.Drain()
	if len(got) != 0 {
		t.Fatalf("second Drain: want 0 items, got %d", len(got))
	}
}

func TestRingBuffer_Wraparound(t *testing.T) {
	rb := newRingBuffer(3)

	rb.Push([]byte("1"))
	rb.Push([]byte("2"))
	rb.Push([]byte("3"))
	rb.Push([]byte("4")) // overwrites "1"

	got := rb.Drain()
	if len(got) != 3 {
		t.Fatalf("Drain after wraparound: want 3 items, got %d", len(got))
	}
	want := []string{"2", "3", "4"}
	for i, w := range want {
		if string(got[i]) != w {
			t.Errorf("Drain[%d]: want %q, got %q", i, w, string(got[i]))
		}
	}
}

func TestRingBuffer_Len(t *testing.T) {
	rb := newRingBuffer(4)
	if rb.Len() != 0 {
		t.Fatalf("empty buffer Len: want 0, got %d", rb.Len())
	}
	rb.Push([]byte("a"))
	rb.Push([]byte("b"))
	if rb.Len() != 2 {
		t.Fatalf("Len after 2 pushes: want 2, got %d", rb.Len())
	}
}

func TestRingBuffer_SingleCapacity(t *testing.T) {
	rb := newRingBuffer(1)
	rb.Push([]byte("first"))
	rb.Push([]byte("second"))

	got := rb.Drain()
	if len(got) != 1 {
		t.Fatalf("want 1 item, got %d", len(got))
	}
	if string(got[0]) != "second" {
		t.Errorf("want %q, got %q", "second", string(got[0]))
	}
}

func TestRingBuffer_ExactCapacity(t *testing.T) {
	rb := newRingBuffer(3)
	rb.Push([]byte("a"))
	rb.Push([]byte("b"))
	rb.Push([]byte("c"))

	got := rb.Drain()
	if len(got) != 3 {
		t.Fatalf("want 3 items, got %d", len(got))
	}
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if string(got[i]) != w {
			t.Errorf("Drain[%d]: want %q, got %q", i, w, string(got[i]))
		}
	}
}

func TestRingBuffer_LenAfterWraparound(t *testing.T) {
	rb := newRingBuffer(2)
	rb.Push([]byte("1"))
	rb.Push([]byte("2"))
	rb.Push([]byte("3")) // wraps

	if rb.Len() != 2 {
		t.Fatalf("Len after wraparound: want 2, got %d", rb.Len())
	}
}
