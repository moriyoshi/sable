package sable

// Regression tests for the empty-result path of the byte-buffer Call ABI.
//
// An empty Rust result buffer makes `Vec::as_ptr` return a dangling-but-aligned
// sentinel (0x1 for u8). Two guards keep that inert: sable_call_result returns a
// NULL ptr for empty buffers, and callResultBytes holds the address as a uintptr
// (never a *C.uint8_t) so Go's GC never scans the sentinel as a heap pointer —
// which would otherwise throw "invalid pointer found on stack: 0x1" when a stack
// copy adjusts the frame while that value is live.

import (
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
)

// TestCallEmptyResult checks the empty-buffer result is delivered correctly for
// both a nil and a zero-length request.
func TestCallEmptyResult(t *testing.T) {
	for _, req := range [][]byte{nil, {}} {
		got, err := Call(OpEcho, req)
		if err != nil {
			t.Fatalf("Call(OpEcho, %v): unexpected err %v", req, err)
		}
		if len(got) != 0 {
			t.Fatalf("Call(OpEcho, %v): got %q, want empty", req, got)
		}
	}
}

// TestCallEmptyResultGCStress hammers the empty-result path from many goroutines
// while forcing aggressive GC and stack growth, so the runtime is repeatedly
// scanning and copying stacks of goroutines parked inside callResultBytes. A
// regression that reverted the ptr to a *C.uint8_t would, with the default
// invalidptr checker on, fault here on the 0x1 sentinel.
func TestCallEmptyResultGCStress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping GC stress in -short")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(5)) // very aggressive GC for the loop

	const goroutines, iters = 32, 400
	var wg sync.WaitGroup
	errs := make(chan string, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				// Enter the FFI from a deep, freshly-grown stack so a stack copy
				// is likely to land while the empty-result address is live.
				got, err := deepCallEcho(24, nil)
				if err != nil || len(got) != 0 {
					errs <- "empty Call under GC stress: unexpected result"
					return
				}
				if i%32 == 0 {
					runtime.GC()
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// deepCallEcho recurses to force this goroutine's stack to grow (and be copied)
// before making the Call, maximizing the chance that a stack adjustment overlaps
// the window in which callResultBytes holds the result address.
//
//go:noinline
func deepCallEcho(depth int, req []byte) ([]byte, error) {
	if depth == 0 {
		return Call(OpEcho, req)
	}
	var pad [512]byte
	pad[0] = byte(depth)
	got, err := deepCallEcho(depth-1, req)
	runtime.KeepAlive(pad)
	return got, err
}
