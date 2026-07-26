//go:build !sable_portable

package sable

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
)

// skipNonLinux skips assertions that are Linux-only by design: the
// rust_awaits_go eventfd value channel (a Go computation awaited over Go's
// netpoll — see rust/src/lib.rs) and the single-shared-epoll invariant
// (countEpollFds reads /proc/self/fd for [eventpoll]). Off Linux these paths
// fall back to a plain Rust-side compute and there is no epoll to count.
func skipNonLinux(t *testing.T, what string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("%s is Linux-only by design", what)
	}
}

func TestMain(m *testing.M) {
	raiseFDLimit() // the Rust-awaits-Go stress creates many transient eventfds
	Init()
	os.Exit(m.Run())
}

// TestBuildRoundTrip is the M0 acceptance test: the Rust staticlib links in and
// a plain call crosses the boundary correctly.
func TestBuildRoundTrip(t *testing.T) {
	if got := sableAdd(40, 2); got != 42 {
		t.Fatalf("sable_add(40, 2) = %d, want 42", got)
	}
}

// TestGoAwaitsRust — direction (i), single shot.
func TestGoAwaitsRust(t *testing.T) {
	if got := awaitRust(0, 0); got != 42 {
		t.Fatalf("awaitRust(0,0) = %d, want 42", got)
	}
}

// TestGoExec exercises the Go-M-driven executor: tasks polled on Go's Ms with
// direct goready delivery. kind 2 = immediate, kind 3 = yield-once (two polls).
// OS-neutral: goexec's per-worker doorbell is the platform-abstracted Doorbell.
func TestGoExec(t *testing.T) {
	if got := awaitGoExec(2, 123); got != 123 {
		t.Fatalf("awaitGoExec(2,123) = %d, want 123", got)
	}
	if got := awaitGoExec(3, 456); got != 456 {
		t.Fatalf("awaitGoExec(3,456) = %d, want 456", got)
	}
	// Concurrent, unique args (detects cross-wiring).
	stress(t, 5000, func(_ uint32, arg uint64) uint64 { return awaitGoExec(2, arg) })
}

// TestStressGoAwaitsRust fans out many concurrent Go->Rust awaits, each echoing
// a unique arg. A wrong/missing/duplicated result would surface as a mismatch,
// a hang (timeout), or a race under -race.
func TestStressGoAwaitsRust(t *testing.T) {
	n := 10000
	if testing.Short() {
		n = 1000
	}
	stress(t, n, awaitRust)
}

// stress runs `n` concurrent awaits of kind 1 (echo arg), asserting every one
// returns exactly its unique arg.
func stress(t *testing.T, n int, await func(kind uint32, arg uint64) uint64) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			arg := uint64(i) + 1 // unique, nonzero
			if got := await(1, arg); got != arg {
				errs <- fmt.Sprintf("await(1, %d) = %d, want %d", arg, got, arg)
			}
		}(i)
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
		t.Fatalf("%d/%d awaits returned wrong results", count, n)
	}
}
