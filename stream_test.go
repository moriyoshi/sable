package sable

// stream_test.go — S-3 end-to-end: open a demo stream, pull each batch lazily as
// a zero-copy handle, read + release it, and confirm ordered delivery and clean
// early-close (cancel).

import (
	"context"
	"testing"
	"time"
	"unsafe"
)

// OpStreamDemo is the demo stream op (Rust `demo` feature): emits req[0] batches,
// each a boxed u64 handle (0xB000 + i).
const OpStreamDemo uint32 = 40

// OpStreamStall is a demo stream that never emits — a Next on it parks until the
// cursor is cancelled or closed.
const OpStreamStall uint32 = 41

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

// TestStreamNextCtxCancel: a Next parked on a stalled stream is unparked promptly
// when its ctx is cancelled from another goroutine (A).
func TestStreamNextCtxCancel(t *testing.T) {
	Init()
	s, err := OpenStream(OpStreamStall, nil)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	ptr, ok := s.NextCtx(ctx)
	if ok {
		demoHandleFree(ptr)
		t.Fatal("NextCtx returned a batch from a stalled stream")
	}
	if ctx.Err() == nil {
		t.Fatal("ctx not cancelled")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("cancel did not unpark Next promptly (%v)", d)
	}
}

// TestOpenStreamBackpressure: with a cap of 1, a second live stream is refused
// until the first is closed (C) — the admission slot is held for the stream's
// lifetime.
func TestOpenStreamBackpressure(t *testing.T) {
	Init()
	SetMaxInFlight(1)
	defer SetMaxInFlight(0)

	s1, err := OpenStream(OpStreamStall, nil) // holds the one slot
	if err != nil {
		t.Fatalf("first OpenStream: %v", err)
	}
	if _, err := OpenStream(OpStreamStall, nil); err != ErrBackpressure {
		s1.Close()
		t.Fatalf("second OpenStream err = %v, want ErrBackpressure", err)
	}
	s1.Close() // releases the slot

	s3, err := OpenStream(OpStreamStall, nil)
	if err != nil {
		t.Fatalf("OpenStream after close: %v", err)
	}
	s3.Close()
}
