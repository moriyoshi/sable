//go:build !sable_portable

package sable

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
)

func TestMain(m *testing.M) {
	raiseFDLimit() // the Rust-awaits-Go stress creates many transient eventfds
	Init()
	os.Exit(m.Run())
}

func raiseFDLimit() {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err == nil {
		lim.Cur = lim.Max
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim)
	}
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

// TestRustAwaitsGo — direction (ii), single shot.
func TestRustAwaitsGo(t *testing.T) {
	if got := demoRustAwaitsGo(0, 0); got != 7 {
		t.Fatalf("demoRustAwaitsGo(0,0) = %d, want 7", got)
	}
}

// TestGoExec exercises the Go-M-driven executor: tasks polled on Go's Ms with
// direct goready delivery. kind 2 = immediate, kind 3 = yield-once (two polls).
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

// TestSingleEpoll is the defining M2 assertion: after exercising both the
// completion doorbell and a fused fd, there is exactly ONE epoll in the process
// (Go's netpoll). tokio, running IO-disabled, owns none.
func TestSingleEpoll(t *testing.T) {
	if got := awaitRust(0, 0); got != 42 { // ensures the poller is initialized
		t.Fatalf("awaitRust = %d, want 42", got)
	}
	if got := demoRustAwaitsGo(0, 0); got != 7 { // exercises GoAsyncFd on the epoll
		t.Fatalf("demoRustAwaitsGo = %d, want 7", got)
	}
	if n := countEpollFds(); n != 1 {
		t.Fatalf("expected exactly 1 epoll fd (Go's netpoll), found %d", n)
	}
}

// TestGoNetpollDrivesRustRead drives a real tokio read whose readiness comes
// only from Go's netpoll, across multiple chunks (exercises the edge-triggered
// re-arm/drain loop).
func TestGoNetpollDrivesRustRead(t *testing.T) {
	const n = 4096
	if got := readPipeViaRust(n, 4); got != uint64(n) {
		t.Fatalf("readPipeViaRust = %d bytes, want %d", got, n)
	}
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

// TestStressRustAwaitsGo fans out many concurrent Go->Rust->Go awaits (both
// directions per iteration). In M2 each iteration registers a fresh fd with the
// netpoller and drives pump control via cgo callbacks serialized on the single
// executor thread, so this is deliberately more modest than the doorbell path.
func TestStressRustAwaitsGo(t *testing.T) {
	n := 2000
	if testing.Short() {
		n = 200
	}
	stress(t, n, awaitRustAwaitsGo)
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
