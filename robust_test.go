//go:build !sable_portable

package sable

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// syncMapLen counts entries in a sync.Map.
func syncMapLen(m interface{ Range(func(k, v any) bool) }) int {
	n := 0
	m.Range(func(_, _ any) bool { n++; return true })
	return n
}

// waitStable polls fn until it returns the same value twice ~50ms apart or the
// deadline passes, then returns the last value. Used to let asynchronous pump
// teardown (which closes fds after pollClose) settle.
func waitStable(fn func() int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	prev := fn()
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		cur := fn()
		if cur == prev {
			return cur
		}
		prev = cur
	}
	return prev
}

// TestNoResourceLeak runs a heavy mixed workload, then asserts that fds return
// to the baseline and the await/pump registries fully drain — this is the
// regression guard for the pump leak and the fd-reuse race.
func TestNoResourceLeak(t *testing.T) {
	countFds := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return -1
		}
		return len(entries)
	}
	// Warm up so lazily-created fds (poller, tokio internals) already exist.
	_ = awaitRust(0, 0)
	base := waitStable(countFds, 2*time.Second)

	const rounds, per = 6, 800
	for r := 0; r < rounds; r++ {
		done := make(chan struct{}, per)
		for i := 0; i < per; i++ {
			go func(i int) {
				arg := uint64(i) + 1
				if i%3 == 0 {
					_ = readPipeViaRust(1024, 2)
				} else {
					_ = awaitRustAwaitsGo(1, arg)
				}
				done <- struct{}{}
			}(i)
		}
		for i := 0; i < per; i++ {
			<-done
		}
	}
	runtime.GC()

	after := waitStable(countFds, 5*time.Second)
	if after > base+8 { // small slack for unrelated runtime fds
		t.Errorf("fd leak: baseline=%d after=%d (delta=%d)", base, after, after-base)
	}
	if n := syncMapLen(&awaits); n != 0 {
		t.Errorf("await registry not drained: %d slots leaked", n)
	}
	if n := syncMapLen(&pumps); n != 0 {
		t.Errorf("pump registry not drained: %d pumps leaked", n)
	}
}

// TestSingleEpollUnderLoad re-asserts the single-shared-epoll invariant after a
// burst of concurrent fused I/O, with the tokio time driver active.
func TestSingleEpollUnderLoad(t *testing.T) {
	done := make(chan struct{}, 400)
	for i := 0; i < 400; i++ {
		go func(i int) {
			_ = awaitRustAwaitsGo(1, uint64(i)+1)
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 400; i++ {
		<-done
	}
	if n := countEpollFds(); n != 1 {
		t.Fatalf("expected exactly 1 epoll under load, found %d", n)
	}
}

// TestRuntimeTeardown exercises create/free repeatedly (executor thread start +
// clean join) and asserts the OS thread count stays bounded (no thread leak).
func TestRuntimeTeardown(t *testing.T) {
	threads := func() int {
		// Threads line in /proc/self/status.
		b, err := os.ReadFile("/proc/self/status")
		if err != nil {
			return -1
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "Threads:") {
				var n int
				_, _ = fmt.Sscanf(line, "Threads:\t%d", &n)
				return n
			}
		}
		return -1
	}
	base := threads()
	for i := 0; i < 30; i++ {
		newAndFreeRuntime()
	}
	// Give joined threads a moment to be reaped.
	time.Sleep(200 * time.Millisecond)
	after := threads()
	if base > 0 && after > base+8 {
		t.Errorf("thread leak across create/free: base=%d after=%d", base, after)
	}
}
