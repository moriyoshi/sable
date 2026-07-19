//go:build !sable_portable

package sable

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// benchCtr yields globally-unique args so throughput benchmarks also detect any
// cross-wiring (a wrong token->slot delivery would echo another arg).
var benchCtr atomic.Uint64

// BenchmarkFusionGoAwaitsRust — raw Go->Rust->Go mutual-await round trips/sec
// (kind 2 = immediate echo, so this measures the fusion machinery: spawn ->
// publish -> doorbell -> dispatcher -> goready, not tokio's timer).
func BenchmarkFusionGoAwaitsRust(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			arg := benchCtr.Add(1)
			if got := awaitRust(2, arg); got != arg {
				b.Fatalf("got %d want %d", got, arg)
			}
		}
	})
}

// BenchmarkGoExec — same immediate round trip, but tasks run on the Go-M-driven
// executor (across Go's Ms) with direct goready delivery. Compare against
// BenchmarkFusionGoAwaitsRust (single dedicated tokio executor thread).
func BenchmarkGoExec(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			arg := benchCtr.Add(1)
			if got := awaitGoExec(2, arg); got != arg {
				b.Fatalf("got %d want %d", got, arg)
			}
		}
	})
}

// CPU-bound comparison: the same ~1µs task under each executor. The dedicated
// tokio thread serializes it (throughput ~= 1 core); goexec spreads it across
// Go's Ms (throughput scales with cores).
func BenchmarkFusionCPU(b *testing.B) { // single dedicated executor thread
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			arg := benchCtr.Add(1)
			if got := awaitRust(4, arg); got != arg {
				b.Fatalf("got %d want %d", got, arg)
			}
		}
	})
}
func BenchmarkGoExecCPU(b *testing.B) { // Go-M-driven executor (parallel)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			arg := benchCtr.Add(1)
			if got := awaitGoExec(4, arg); got != arg {
				b.Fatalf("got %d want %d", got, arg)
			}
		}
	})
}

// TestLatency reports single-in-flight (conc=1) round-trip latency for each
// executor. goexec removes one of the two cross-thread wakeup hops.
func TestLatency(t *testing.T) {
	if os.Getenv("SABLE_STRESS") == "" {
		t.Skip("set SABLE_STRESS=1")
	}
	const N = 20000
	_ = awaitGoExec(2, 1) // warm up the worker pool
	measure := func(name string, f func(uint64) uint64) {
		start := time.Now()
		for i := uint64(1); i <= N; i++ {
			if got := f(i); got != i {
				t.Fatalf("%s: got %d want %d", name, got, i)
			}
		}
		t.Logf("%-26s %.0f ns/await (conc=1)", name, float64(time.Since(start).Nanoseconds())/N)
	}
	measure("dedicated (awaitRust)", func(a uint64) uint64 { return awaitRust(2, a) })
	measure("goexec (awaitGoExec)", func(a uint64) uint64 { return awaitGoExec(2, a) })
}

// BenchmarkFusionRustAwaitsGo — both directions per op (Go->Rust->Go->Rust->Go),
// exercising the netpoll pump + eventfd path under load.
func BenchmarkFusionRustAwaitsGo(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			arg := benchCtr.Add(1)
			if got := awaitRustAwaitsGo(1, arg); got != arg {
				b.Fatalf("got %d want %d", got, arg)
			}
		}
	})
}

// TestScaling reports sustained throughput at several concurrency levels. Run
// with -v to see the table. The single-threaded tokio executor is the core
// bottleneck, so this shows how throughput saturates as concurrency rises.
func TestScaling(t *testing.T) {
	if os.Getenv("SABLE_STRESS") == "" {
		t.Skip("set SABLE_STRESS=1 to run scaling/soak stress")
	}
	const perLevel = 200_000
	t.Logf("%-8s %-14s %-12s", "conc", "awaits/sec", "ns/await")
	for _, conc := range []int{1, 8, 64, 512, 4096, 32768} {
		var next atomic.Uint64
		var wg sync.WaitGroup
		start := time.Now()
		for w := 0; w < conc; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					n := next.Add(1)
					if n > perLevel {
						return
					}
					if got := awaitRust(2, n); got != n {
						t.Errorf("got %d want %d", got, n)
						return
					}
				}
			}()
		}
		wg.Wait()
		el := time.Since(start)
		rate := float64(perLevel) / el.Seconds()
		t.Logf("%-8d %-14.0f %-12.0f", conc, rate, float64(el.Nanoseconds())/perLevel)
	}
}

// TestSoak drives a large sustained workload (default 3M mixed awaits across
// many workers) and asserts correctness + no resource leak. Set SABLE_STRESS=1;
// override the total with SABLE_SOAK_N.
func TestSoak(t *testing.T) {
	if os.Getenv("SABLE_STRESS") == "" {
		t.Skip("set SABLE_STRESS=1 to run scaling/soak stress")
	}
	total := 3_000_000
	if v := os.Getenv("SABLE_SOAK_N"); v != "" {
		fmt.Sscanf(v, "%d", &total)
	}
	const workers = 4096

	// Warm up both executors' fixed pools (dispatcher, goexec worker pool +
	// doorbell fds) BEFORE snapshotting the baseline, so the leak check measures
	// per-task growth, not one-time init.
	_ = awaitRust(2, 1)
	_ = awaitGoExec(2, 1)
	baseFd := waitStable(fdCount, 2*time.Second)
	baseGo := runtime.NumGoroutine()

	var next, bad atomic.Uint64
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n := next.Add(1)
				if n > uint64(total) {
					return
				}
				// Mix both directions: 3/4 fast Go->Rust, 1/4 Rust-awaits-Go.
				// SABLE_EXEC routes the fast path through the goexec (Go-M) path.
				var got uint64
				if n%4 == 0 {
					got = awaitRustAwaitsGo(1, n)
				} else if os.Getenv("SABLE_EXEC") != "" {
					got = awaitGoExec(2, n)
				} else {
					got = awaitRust(2, n)
				}
				if got != n {
					bad.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	el := time.Since(start)

	if bad.Load() != 0 {
		t.Fatalf("%d/%d awaits returned wrong results", bad.Load(), total)
	}
	t.Logf("soak: %d awaits in %s = %.0f awaits/sec", total, el.Round(time.Millisecond),
		float64(total)/el.Seconds())

	// Let asynchronous pump/fd teardown settle, then check for leaks.
	runtime.GC()
	afterFd := waitStable(fdCount, 5*time.Second)
	afterGo := waitStable(runtime.NumGoroutine, 5*time.Second)
	t.Logf("fds: %d -> %d ; goroutines: %d -> %d ; registries: awaits=%d pumps=%d",
		baseFd, afterFd, baseGo, afterGo, syncMapLen(&awaits), syncMapLen(&pumps))
	if afterFd > baseFd+8 {
		t.Errorf("fd leak: %d -> %d", baseFd, afterFd)
	}
	if syncMapLen(&awaits) != 0 || syncMapLen(&pumps) != 0 {
		t.Errorf("registry leak: awaits=%d pumps=%d", syncMapLen(&awaits), syncMapLen(&pumps))
	}
}

func fdCount() int {
	e, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(e)
}
