package sable

// handle_test.go — S-2 end-to-end: a registered Rust handler returns an opaque
// handle, sable delivers the raw pointer on the existing completion path, and Go
// takes ownership + drives release. Works under both backends (core path).

import (
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

// TestCallHandleUnknownOp: an op with no handler (and not a demo handle op)
// yields no handle, reported as ErrNoHandle rather than a bogus pointer.
func TestCallHandleUnknownOp(t *testing.T) {
	Init()
	if _, err := CallHandle(9999, nil); err != ErrNoHandle {
		t.Fatalf("CallHandle(unknown) err = %v, want ErrNoHandle", err)
	}
}
