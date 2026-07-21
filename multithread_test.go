//go:build sable_multithread

package sable

// multithread_test.go — S-4.2 end-to-end: with the runtime built on the
// IO-disabled multi-thread executor (-tags sable_multithread + Rust --features
// multithread), the fusion still works AND the process still holds exactly one
// epoll (Go's netpoll). Run via `make test-multithread`.

import (
	"sync"
	"testing"
)

func TestMultithreadSingleEpoll(t *testing.T) {
	Init()
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			arg := uint64(i) + 1
			if got := AwaitRust(1, arg); got != arg { // kind 1: echo arg
				t.Errorf("AwaitRust(1,%d) = %d", arg, got)
			}
		}(i)
	}
	wg.Wait()
	if got := countEpollFds(); got != 1 {
		t.Fatalf("epoll fds = %d, want 1 (single epoll under the multi-thread executor)", got)
	}
}
