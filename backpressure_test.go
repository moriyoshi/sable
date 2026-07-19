package sable

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitInFlightZero(t *testing.T) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if RuntimeStats().InFlight == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("runtime did not quiesce: in_flight=%d", RuntimeStats().InFlight)
}

// TestBackpressure: with a low in-flight cap and a burst of simultaneous TryCall
// admissions, some are admitted and the rest refused with ErrBackpressure; the
// in-flight gauge never exceeds the cap; no calls are lost; the rejected counter
// advances; and the gauge drains to zero afterward.
func TestBackpressure(t *testing.T) {
	defer SetMaxInFlight(0) // restore unbounded for the rest of the suite
	waitInFlightZero(t)

	const cap = 4
	SetMaxInFlight(cap)
	if got := RuntimeStats().MaxInFlight; got != cap {
		t.Fatalf("MaxInFlight = %d, want %d", got, cap)
	}
	baseRejected := RuntimeStats().Rejected

	// Sample the in-flight gauge to prove it never exceeds the cap under a
	// TryCall-only workload.
	var maxObserved uint64
	stop := make(chan struct{})
	var samplerDone sync.WaitGroup
	samplerDone.Add(1)
	go func() {
		defer samplerDone.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if f := RuntimeStats().InFlight; f > atomic.LoadUint64(&maxObserved) {
				atomic.StoreUint64(&maxObserved, f)
			}
			time.Sleep(50 * time.Microsecond)
		}
	}()

	// Release N goroutines simultaneously so they contend for the cap at once.
	const N = 300
	start := make(chan struct{})
	var admitted, rejected int64
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := TryCall(OpDelayEcho, []byte("x")) // ~1ms op, stays in-flight
			switch {
			case err == nil:
				atomic.AddInt64(&admitted, 1)
			case errors.Is(err, ErrBackpressure):
				atomic.AddInt64(&rejected, 1)
			default:
				t.Errorf("unexpected err: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(stop)
	samplerDone.Wait()

	if admitted == 0 {
		t.Errorf("expected some admissions, got 0")
	}
	if rejected == 0 {
		t.Errorf("expected rejections at cap=%d under %d simultaneous calls, got 0", cap, N)
	}
	if admitted+rejected != N {
		t.Errorf("lost calls: admitted %d + rejected %d != %d", admitted, rejected, N)
	}
	if mo := atomic.LoadUint64(&maxObserved); mo > cap {
		t.Errorf("in_flight peaked at %d, exceeding cap %d", mo, cap)
	}
	if adv := RuntimeStats().Rejected - baseRejected; adv < uint64(rejected) {
		t.Errorf("rejected counter advanced %d, want >= %d", adv, rejected)
	}
	waitInFlightZero(t)

	// With the cap lifted, the same burst is fully admitted (no rejections).
	SetMaxInFlight(0)
	base2 := RuntimeStats().Rejected
	var wg2 sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			if _, err := TryCall(OpEcho, []byte("y")); err != nil {
				t.Errorf("unbounded TryCall errored: %v", err)
			}
		}()
	}
	wg2.Wait()
	if RuntimeStats().Rejected != base2 {
		t.Errorf("unbounded TryCall produced rejections")
	}
}
