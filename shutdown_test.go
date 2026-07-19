package sable

import (
	"os"
	"runtime"
	"testing"
	"time"
)

func countOpenFds(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return len(ents)
}

// TestTeardownNoLeak creates and frees many runtimes WITH tasks in flight and
// asserts neither open file descriptors (the completion eventfd) nor goroutines
// grow — i.e. every runtime's eventfd is closed and every executor thread joins.
// This is the regression guard for the eventfd leak (Inner had no Drop).
func TestTeardownNoLeak(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fd-leak check reads /proc/self/fd (Linux-only); see README Operating-system support")
	}
	// Warm up: first cycles may allocate lazily-initialized caches.
	for i := 0; i < 5; i++ {
		spawnAndFreeRuntime(8)
	}
	time.Sleep(20 * time.Millisecond)
	runtime.GC()

	fdsBefore := countOpenFds(t)
	gorBefore := runtime.NumGoroutine()

	const cycles = 100
	for i := 0; i < cycles; i++ {
		spawnAndFreeRuntime(8)
	}
	time.Sleep(50 * time.Millisecond) // let any transient fds/threads settle
	runtime.GC()

	fdsAfter := countOpenFds(t)
	gorAfter := runtime.NumGoroutine()

	// Each runtime opens exactly one eventfd; if it leaked, this would be +100.
	if fdsAfter > fdsBefore+3 {
		t.Errorf("fd leak across %d new/free cycles: %d -> %d", cycles, fdsBefore, fdsAfter)
	}
	if gorAfter > gorBefore+3 {
		t.Errorf("goroutine leak across %d new/free cycles: %d -> %d", cycles, gorBefore, gorAfter)
	}
}

// TestShutdownAndReinit exercises the full global-runtime shutdown ordering
// (abort+join -> stop dispatcher -> free) and then re-initializes so the rest of
// the suite has a live runtime. Proves the dispatcher stops cleanly (no
// use-after-free of the freed rt) and the runtime is fully re-creatable.
func TestShutdownAndReinit(t *testing.T) {
	if _, err := Call(OpEcho, []byte("pre")); err != nil {
		t.Fatalf("Call before shutdown: %v", err)
	}

	done := dispatcherDone
	Shutdown()

	if rt != nil {
		t.Errorf("rt should be nil after Shutdown")
	}
	select {
	case <-done:
		// dispatcher exited
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not exit after Shutdown")
	}

	// Re-init and confirm the fused runtime works again.
	sableInit()
	if _, err := Call(OpEcho, []byte("post")); err != nil {
		t.Fatalf("Call after reinit: %v", err)
	}
	// And a second Shutdown+reinit to confirm idempotency of the sequence.
	Shutdown()
	sableInit()
	if got, err := Call(OpUpper, []byte("ab")); err != nil || string(got) != "AB" {
		t.Fatalf("Call after 2nd reinit: got %q err %v", got, err)
	}
}
