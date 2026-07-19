//go:build !sable_portable

package sable

import (
	"fmt"
	"sync"
	"testing"
)

// crossSink keeps the compiler from optimizing away benchmarked results. Used by
// the in-package benchmarks that measure unexported internals (inline paths); the
// public-API microbenchmarks live in the separate bench/ package.
var crossSink uint64

func TestInline(t *testing.T) {
	// Fast path (compute, never suspends).
	if got := awaitInline(2, 99); got != 99 {
		t.Fatalf("awaitInline(2,99)=%d want 99", got)
	}
	if got := awaitInline(4, 77); got != 77 {
		t.Fatalf("awaitInline(4,77)=%d want 77", got)
	}
	// Pending fallback (kind 5 suspends awaiting a Go computation over netpoll).
	if got := awaitInline(5, 12345); got != 12345 {
		t.Fatalf("awaitInline(5,12345)=%d want 12345 (fallback path)", got)
	}
	// Concurrent fallback, unique args (detects lost/cross-wired wakeups).
	var wg sync.WaitGroup
	errs := make(chan string, 2000)
	for i := 0; i < 2000; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			arg := uint64(i) + 1
			if got := awaitInline(5, arg); got != arg {
				errs <- fmt.Sprintf("fallback arg %d -> %d", arg, got)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// Fast-path floor (one-shot, no fallback machinery).
func BenchmarkInlineFloor(b *testing.B) {
	var s uint64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s += awaitInlineOneshot(2, 1)
		}
	})
	crossSink = s
}

// Full M7 path, fast case (compute → inline poll returns Ready).
func BenchmarkInlineTrivial(b *testing.B) {
	var s uint64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s += awaitInline(2, 1)
		}
	})
	crossSink = s
}
func BenchmarkInlineCPU(b *testing.B) {
	var s uint64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s += awaitInline(4, 1)
		}
	})
	crossSink = s
}

// Full M7 path, fallback case (kind 5 suspends → park/deliver/re-poll).
func BenchmarkInlineFallback(b *testing.B) {
	var s uint64
	b.RunParallel(func(pb *testing.PB) {
		var i uint64
		for pb.Next() {
			i++
			s += awaitInline(5, i)
		}
	})
	crossSink = s
}
