package sable

// call.go — the generic byte-buffer Call API (R3). A Go goroutine awaits an
// arbitrary Rust async operation; request and response are byte buffers (the
// caller serializes its own types), with an ok/err flag. Core — works under both
// the fast and the portable backends.

// #include "sable.h"
import "C"

import (
	"context"
	"errors"
	"runtime"
	"unsafe"
)

// Built-in demo op ids (a real library would use a user-populated registry).
const (
	OpEcho      uint32 = 0
	OpUpper     uint32 = 1
	OpLen       uint32 = 2
	OpFail      uint32 = 3
	OpDelayEcho uint32 = 4
)

// Call awaits Rust op `op` with request bytes `req`, returning the response
// bytes or an error carrying the Rust error bytes. The awaiting goroutine parks
// (gopark or channel, per backend) until the async handler completes.
func Call(op uint32, req []byte) ([]byte, error) {
	Init()
	var reqPtr *C.uint8_t
	if len(req) > 0 {
		reqPtr = (*C.uint8_t)(unsafe.Pointer(&req[0]))
	}
	// spawn() (run synchronously inside awaitViaPark) copies req on the Rust side.
	handle := awaitViaPark(func(token uint64) {
		C.sable_call(rt, C.uint32_t(op), reqPtr, C.size_t(len(req)), C.uint64_t(token))
	})
	runtime.KeepAlive(req)
	return callResultBytes(handle)
}

// AwaitCallResult parks the calling goroutine on a fresh completion token, runs
// spawn (which must kick off a Rust task that completes with a CallResult buffer
// on that token), then copies out and frees the result — returning the response
// bytes or an error carrying the Rust error bytes. It is the byte-buffer
// analogue of AwaitToken, for advanced examples whose Rust handler returns a
// serialized value (e.g. the real rust2go g2r binding; see examples/rust2go-real).
func AwaitCallResult(spawn func(token uint64)) ([]byte, error) {
	Init()
	return callResultBytes(awaitViaPark(spawn))
}

// callResultBytes copies out and frees the Rust-owned CallResult for handle.
//
// `ptr` is held as a uintptr, not a *C.uint8_t: the Rust buffer is C-owned memory the Go GC must never
// scan as a heap pointer. An empty result in particular can carry a sub-page sentinel (a dangling
// Vec::as_ptr, e.g. 0x1) that the GC rejects as an "invalid pointer found on stack" if it lands in a
// pointer-typed stack slot. A uintptr is not scanned, so any bit pattern is inert; we materialize an
// unsafe.Pointer only transiently for the copy, and only when there are bytes to copy.
func callResultBytes(handle uint64) ([]byte, error) {
	var ok C.int
	var ptr uintptr
	var n C.size_t
	C.sable_call_result(C.uint64_t(handle), &ok, (**C.uint8_t)(unsafe.Pointer(&ptr)), &n)
	var out []byte
	if n > 0 && ptr != 0 {
		out = C.GoBytes(unsafe.Pointer(ptr), C.int(n))
	}
	C.sable_call_free(C.uint64_t(handle))
	if ok == 0 {
		return nil, errors.New(string(out))
	}
	return out, nil
}

// ErrBackpressure is returned by TryCall when the runtime is at its in-flight
// cap (see SetMaxInFlight). No task was spawned; the caller should shed load,
// retry with backoff, or apply its own admission control.
var ErrBackpressure = errors.New("sable: at in-flight capacity")

// SetMaxInFlight caps the number of concurrently in-flight tasks the TryCall
// path admits (0 = unbounded, the default). Unbounded entry points (Call,
// CallCtx) still count toward the gauge but are never refused.
func SetMaxInFlight(max uint64) {
	Init()
	C.sable_set_max_in_flight(rt, C.uint64_t(max))
}

// TryCall is Call with backpressure: if the runtime is already at its in-flight
// cap it returns ErrBackpressure immediately (nothing spawned, nothing parked)
// instead of piling on unbounded work. Otherwise it behaves exactly like Call.
func TryCall(op uint32, req []byte) ([]byte, error) {
	Init()
	var reqPtr *C.uint8_t
	if len(req) > 0 {
		reqPtr = (*C.uint8_t)(unsafe.Pointer(&req[0]))
	}
	handle, admitted := awaitViaParkTry(func(token uint64) bool {
		return C.sable_call_try(rt, C.uint32_t(op), reqPtr, C.size_t(len(req)), C.uint64_t(token)) != 0
	})
	runtime.KeepAlive(req)
	if !admitted {
		return nil, ErrBackpressure
	}
	return callResultBytes(handle)
}

// CallCtx is Call with cancellation: if ctx is cancelled before the Rust op
// completes, the in-flight tokio task is aborted (its future dropped, running
// its cleanup) and CallCtx returns ctx.Err(). Best-effort — if the op finishes
// first, its result is returned.
func CallCtx(ctx context.Context, op uint32, req []byte) ([]byte, error) {
	Init()
	var reqPtr *C.uint8_t
	if len(req) > 0 {
		reqPtr = (*C.uint8_t)(unsafe.Pointer(&req[0]))
	}
	stop := make(chan struct{})
	handle := awaitViaPark(func(token uint64) {
		C.sable_call_ctx(rt, C.uint32_t(op), reqPtr, C.size_t(len(req)), C.uint64_t(token))
		// Watcher: abort the Rust task if ctx is cancelled before delivery.
		go func() {
			select {
			case <-ctx.Done():
				C.sable_call_cancel(C.uint64_t(token))
			case <-stop:
			}
		}()
	})
	close(stop) // delivered (result or cancel); stop the watcher
	runtime.KeepAlive(req)

	if handle == 0 { // cancellation sentinel
		return nil, ctx.Err()
	}
	return callResultBytes(handle)
}

const OpSleepLong uint32 = 5

func callPending() uint64 { return uint64(C.sable_call_pending()) }

// ErrNoHandle is returned by CallHandle when the handler produced no handle
// (it errored or returned bytes), signalled by a 0 completion result. The caller
// can re-run the op via Call to retrieve the error bytes.
var ErrNoHandle = errors.New("sable: handler returned no handle")

// CallHandle awaits Rust op `op` on the zero-copy handle path (S-2): the
// registered handler (S-1) returns an opaque pointer — e.g. a
// *mut FFI_ArrowArrayStream — which sable delivers verbatim. Go takes ownership,
// disarming sable's release-on-drop net, and MUST drive the pointer's own
// release itself (for an Arrow stream, cdata.ImportCArrayStream does this on
// Release()). Returns ErrNoHandle if the handler produced no handle.
func CallHandle(op uint32, req []byte) (uintptr, error) {
	Init()
	var reqPtr *C.uint8_t
	if len(req) > 0 {
		reqPtr = (*C.uint8_t)(unsafe.Pointer(&req[0]))
	}
	var tok C.uint64_t
	ptr := awaitViaPark(func(token uint64) {
		tok = C.uint64_t(token)
		C.sable_call_handle(rt, C.uint32_t(op), reqPtr, C.size_t(len(req)), C.uint64_t(token))
	})
	runtime.KeepAlive(req)
	if ptr == 0 {
		return 0, handleError(tok) // handler error (read without re-running) or ErrNoHandle
	}
	C.sable_call_handle_taken(tok) // take ownership; disarm sable's release net
	return uintptr(ptr), nil
}

// CallHandleCtx is CallHandle with cancellation: if ctx is cancelled before the
// handler produces its handle, the in-flight tokio task is aborted and CallHandleCtx
// returns ctx.Err(). Best-effort — if the handle lands first, it is returned.
func CallHandleCtx(ctx context.Context, op uint32, req []byte) (uintptr, error) {
	Init()
	var reqPtr *C.uint8_t
	if len(req) > 0 {
		reqPtr = (*C.uint8_t)(unsafe.Pointer(&req[0]))
	}
	stop := make(chan struct{})
	var tok C.uint64_t
	ptr := awaitViaPark(func(token uint64) {
		tok = C.uint64_t(token)
		C.sable_call_handle_ctx(rt, C.uint32_t(op), reqPtr, C.size_t(len(req)), C.uint64_t(token))
		go func() {
			select {
			case <-ctx.Done():
				C.sable_call_cancel(C.uint64_t(token))
			case <-stop:
			}
		}()
	})
	close(stop)
	runtime.KeepAlive(req)
	if ptr == 0 {
		if err := ctx.Err(); err != nil {
			return 0, err // cancelled
		}
		return 0, handleError(tok)
	}
	C.sable_call_handle_taken(tok)
	return uintptr(ptr), nil
}

// handleError reads the error recorded on the handle path for a 0 result (D) —
// without re-running the op — or ErrNoHandle if there was none.
func handleError(tok C.uint64_t) error {
	h := uint64(C.sable_call_handle_error(tok))
	if h == 0 {
		return ErrNoHandle
	}
	if _, err := callResultBytes(h); err != nil {
		return err
	}
	return ErrNoHandle
}
