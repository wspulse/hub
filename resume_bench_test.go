package wspulse

import "testing"

func BenchmarkRingBuffer_Push(b *testing.B) {
	rb := newRingBuffer(256)
	frame := []byte(`{"event":"bench","payload":{}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rb.Push(frame)
	}
}

func BenchmarkRingBuffer_PushDrain(b *testing.B) {
	rb := newRingBuffer(256)
	frame := []byte(`{"event":"bench","payload":{}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 256; j++ {
			rb.Push(frame)
		}
		rb.Drain()
	}
}
