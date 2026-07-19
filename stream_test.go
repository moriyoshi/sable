package sable

// stream_test.go — S-3 end-to-end: open a demo stream, pull each batch lazily as
// a zero-copy handle, read + release it, and confirm ordered delivery and clean
// early-close (cancel).

import (
	"testing"
	"unsafe"
)

// OpStreamDemo is the demo stream op (Rust `demo` feature): emits req[0] batches,
// each a boxed u64 handle (0xB000 + i).
const OpStreamDemo uint32 = 40

func TestStreamFullDrain(t *testing.T) {
	Init()
	const n = 5
	s, err := OpenStream(OpStreamDemo, []byte{n})
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	got := 0
	for {
		ptr, ok := s.Next()
		if !ok {
			break
		}
		if v := *(*uint64)(unsafe.Pointer(ptr)); v != 0xB000+uint64(got) {
			t.Fatalf("batch %d = %#x, want %#x", got, v, 0xB000+uint64(got))
		}
		demoHandleFree(ptr) // Go owns the batch; drive its release
		got++
	}
	s.Close()
	if got != n {
		t.Fatalf("pulled %d batches, want %d", got, n)
	}
}

// TestStreamEarlyClose cancels mid-stream: Close must abort the producer and
// release buffered batches without leaking or deadlocking.
func TestStreamEarlyClose(t *testing.T) {
	Init()
	s, err := OpenStream(OpStreamDemo, []byte{100})
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	// Pull just two batches, then close early.
	for i := 0; i < 2; i++ {
		ptr, ok := s.Next()
		if !ok {
			t.Fatalf("batch %d: unexpected end of stream", i)
		}
		demoHandleFree(ptr)
	}
	s.Close()
	// After close, Next reports end-of-stream.
	if _, ok := s.Next(); ok {
		t.Fatal("Next after Close returned a batch")
	}
}

func TestStreamUnknownOp(t *testing.T) {
	Init()
	if _, err := OpenStream(9999, nil); err != ErrNoStream {
		t.Fatalf("OpenStream(unknown) err = %v, want ErrNoStream", err)
	}
}
