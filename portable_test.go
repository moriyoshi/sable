//go:build sable_portable

package sable

import (
	"os"
	"sync"
	"testing"
)

func TestMain(m *testing.M) {
	Init()
	os.Exit(m.Run())
}

// TestPortableAwait: the core "Go awaits Rust" fusion works via the channel
// backend, with zero //go:linkname in the build.
func TestPortableAwait(t *testing.T) {
	if got := awaitRust(0, 0); got != 42 { // kind 0: sleep 1ms, return 42
		t.Fatalf("awaitRust(0,0) = %d, want 42", got)
	}
	// Concurrent, unique args — detects lost/cross-wired deliveries.
	const n = 2000
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			arg := uint64(i) + 1
			if got := awaitRust(1, arg); got != arg { // kind 1: echo arg
				errs <- "mismatch"
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
