package sable

// stream.go — S-3: the streaming Call API. A stream op yields many batches
// lazily; Go opens a server-side cursor and pulls one batch per await (the
// goroutine parks between batches — no OS thread blocked, no block_on in Rust).
// Per-batch handles are zero-copy, owned by Go once taken, exactly as for the
// one-shot handle path (CallHandle).

// #include "sable.h"
import "C"

import (
	"context"
	"errors"
	"runtime"
	"unsafe"
)

// ErrNoStream is returned by OpenStream when `op` has no registered stream
// handler (cursor id came back 0).
var ErrNoStream = errors.New("sable: no stream handler for op")

// Stream is an open server-side cursor. Pull batches with Next until it reports
// end-of-stream, then call Close (Close is also how you cancel early). Not safe
// for concurrent use: iterate from one goroutine.
type Stream struct {
	cursor uint64
	closed bool
}

// OpenStream opens a streaming op, returning a cursor to pull batches from. It is
// admission-controlled: at the in-flight cap (see SetMaxInFlight) it returns
// ErrBackpressure without opening a stream — the admission slot is held for the
// stream's lifetime, so the cap bounds concurrent live streams. Always Close a
// returned Stream to release its slot.
func OpenStream(op uint32, req []byte) (*Stream, error) {
	Init()
	var reqPtr *C.uint8_t
	if len(req) > 0 {
		reqPtr = (*C.uint8_t)(unsafe.Pointer(&req[0]))
	}
	cursor, admitted := awaitViaParkTry(func(token uint64) bool {
		return C.sable_stream_open(rt, C.uint32_t(op), reqPtr, C.size_t(len(req)), C.uint64_t(token)) != 0
	})
	runtime.KeepAlive(req)
	if !admitted {
		return nil, ErrBackpressure
	}
	if cursor == 0 {
		return nil, ErrNoStream
	}
	return &Stream{cursor: cursor}, nil
}

// Next pulls the next batch. ok=false marks end-of-stream (no handle; the cursor
// is exhausted — call Close). On ok=true the returned pointer is a zero-copy,
// Go-owned batch handle (e.g. a *FFI_ArrowArray); the caller MUST drive its
// release itself, as with CallHandle.
func (s *Stream) Next() (ptr uintptr, ok bool) {
	if s.closed {
		return 0, false
	}
	var tok C.uint64_t
	p := awaitViaPark(func(token uint64) {
		tok = C.uint64_t(token)
		C.sable_stream_next(rt, C.uint64_t(s.cursor), C.uint64_t(token))
	})
	if p == 0 {
		return 0, false // end of stream
	}
	C.sable_call_handle_taken(tok) // take ownership of the batch; disarm the net
	return uintptr(p), true
}

// NextCtx is Next with cancellation: if ctx is cancelled while parked awaiting a
// slow batch, the pull is aborted and NextCtx returns ok=false. This is the
// race-safe way to interrupt a parked pull — a distinct goroutine cancelling ctx
// unblocks it. ok=false means end-of-stream OR cancellation; check ctx.Err() to
// distinguish. After a cancellation, Close the stream.
func (s *Stream) NextCtx(ctx context.Context) (ptr uintptr, ok bool) {
	if s.closed {
		return 0, false
	}
	stop := make(chan struct{})
	var tok C.uint64_t
	p := awaitViaPark(func(token uint64) {
		tok = C.uint64_t(token)
		C.sable_stream_next_ctx(rt, C.uint64_t(s.cursor), C.uint64_t(token))
		go func() {
			select {
			case <-ctx.Done():
				C.sable_call_cancel(C.uint64_t(token))
			case <-stop:
			}
		}()
	})
	close(stop)
	if p == 0 {
		return 0, false // end-of-stream or cancelled (caller checks ctx.Err())
	}
	C.sable_call_handle_taken(tok)
	return uintptr(p), true
}

// Close releases the cursor: aborts the producer (dropping whatever it pinned)
// and frees any buffered batches. Idempotent; safe to call before end-of-stream
// to cancel.
func (s *Stream) Close() {
	if s.closed {
		return
	}
	s.closed = true
	C.sable_stream_close(C.uint64_t(s.cursor))
}
