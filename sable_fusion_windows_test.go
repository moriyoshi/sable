//go:build !sable_portable && windows

package sable

// sable_fusion_windows_test.go — acceptance tests for Windows IOCP fd fusion (the
// full Rust-FFI path). The read mechanism's "goes through the single IOCP, not
// thread-per-read" invariant is proven directly on os.NewFile by
// TestSpikeBoundedThreads (spike_overlapped_windows_test.go); these exercise the
// end-to-end Go-reads-→-Rust-awaits-count contract.

import (
	"fmt"
	"sync"
	"testing"
)

// TestGoDrivesRustReadWindows: a Rust task awaits a 4096-byte pipe read whose
// bytes Go reads through the runtime IOCP across several dribbled completions.
func TestGoDrivesRustReadWindows(t *testing.T) {
	const n = 4096
	if got := readPipeViaRust(n, 4); got != uint64(n) {
		t.Fatalf("readPipeViaRust(%d,4) = %d, want %d", n, got, n)
	}
}

// TestGoDrivesRustReadPartialEOF: the writer sends fewer bytes than the reader
// wants and closes; the reader must deliver the partial count (EOF path).
func TestGoDrivesRustReadPartialEOF(t *testing.T) {
	const want, have = 4096, 2048
	if got := readPipeViaRustSplit(want, have, 2); got != uint64(have) {
		t.Fatalf("partial read = %d, want %d", got, have)
	}
}

// TestStressGoDrivesRustRead fans out many concurrent fused reads, each echoing
// its exact byte count. A wrong/missing/duplicated completion surfaces as a
// mismatch or a hang (timeout).
func TestStressGoDrivesRustRead(t *testing.T) {
	n := 200
	if testing.Short() {
		n = 50
	}
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := readPipeViaRust(1024, 2); got != 1024 {
				errs <- fmt.Sprintf("got %d want 1024", got)
			}
		}()
	}
	wg.Wait()
	close(errs)
	count := 0
	for e := range errs {
		if count < 10 {
			t.Error(e)
		}
		count++
	}
	if count > 0 {
		t.Fatalf("%d/%d fused reads returned wrong counts", count, n)
	}
}
