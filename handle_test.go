package sable

// handle_test.go — S-2 end-to-end: a registered Rust handler returns an opaque
// handle, sable delivers the raw pointer on the existing completion path, and Go
// takes ownership + drives release. Works under both backends (core path).

import (
	"context"
	"strings"
	"testing"
	"unsafe"
)

// OpHandleDemo is the demo handle op (Rust `demo` feature): returns a boxed u64
// marker as an opaque handle.
const OpHandleDemo uint32 = 30

func TestCallHandle(t *testing.T) {
	Init()
	ptr, err := CallHandle(OpHandleDemo, nil)
	if err != nil {
		t.Fatalf("CallHandle: %v", err)
	}
	if ptr == 0 {
		t.Fatal("CallHandle returned a nil handle")
	}
	// Zero-copy read: the pointer is Rust-owned memory sable delivered verbatim.
	if got := *(*uint64)(unsafe.Pointer(ptr)); got != 0x00C0FFEE {
		t.Fatalf("handle payload = %#x, want 0x00C0FFEE", got)
	}
	// Go owns it now (sable's net was disarmed by CallHandle) — drive release.
	demoHandleFree(ptr)
}

// TestCallHandleUnknownOp: an unknown op yields no handle, and the handler's
// error is delivered on the handle path (D) — without re-running the op — rather
// than a generic sentinel.
func TestCallHandleUnknownOp(t *testing.T) {
	Init()
	ptr, err := CallHandle(9999, nil)
	if ptr != 0 {
		t.Fatalf("CallHandle(unknown) ptr = %#x, want 0", ptr)
	}
	if err == nil || !strings.Contains(err.Error(), "unknown op") {
		t.Fatalf("CallHandle(unknown) err = %v, want an 'unknown op' error", err)
	}
}

// TestCallHandleCtxCancel: cancelling the ctx before the handle lands returns
// ctx.Err(), not a handle.
func TestCallHandleCtxCancel(t *testing.T) {
	Init()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	ptr, err := CallHandleCtx(ctx, OpSleepLong, nil)
	if ptr != 0 {
		demoHandleFree(ptr)
		t.Fatalf("CallHandleCtx(cancelled) ptr = %#x, want 0", ptr)
	}
	if err != context.Canceled {
		t.Fatalf("CallHandleCtx(cancelled) err = %v, want context.Canceled", err)
	}
}
