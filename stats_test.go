package sable

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestStats verifies the observability counters track real work: spawned and
// completed advance together for finished calls, in-flight drains to zero, and
// a cancellation is both counted and reflected in the cancel registry draining.
func TestStats(t *testing.T) {
	base := RuntimeStats()

	// A batch of completed calls: spawned and completed both advance by N,
	// in-flight returns to its baseline.
	const N = 200
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Call(OpEcho, []byte("x")); err != nil {
				t.Errorf("Call: %v", err)
			}
		}()
	}
	wg.Wait()

	got := RuntimeStats()
	if d := got.Spawned - base.Spawned; d < N {
		t.Errorf("spawned advanced by %d, want >= %d", d, N)
	}
	if d := got.Completed - base.Completed; d < N {
		t.Errorf("completed advanced by %d, want >= %d", d, N)
	}
	// After a barrier where every call returned, in-flight should have settled.
	settled := false
	for i := 0; i < 100; i++ {
		if RuntimeStats().InFlight == 0 {
			settled = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !settled {
		t.Errorf("in_flight did not settle to 0: %d", RuntimeStats().InFlight)
	}

	// A cancellation is counted and the registry drains.
	preCancel := RuntimeStats()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = CallCtx(ctx, OpSleepLong, []byte("y"))
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	// While the 10s op is in flight it should be registered as cancellable.
	if RuntimeStats().CancelsRegistered == 0 {
		t.Errorf("in-flight cancellable call not registered")
	}
	cancel()
	<-done
	for i := 0; i < 100 && RuntimeStats().CancelsRegistered != 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	post := RuntimeStats()
	if post.Cancelled <= preCancel.Cancelled {
		t.Errorf("cancelled counter did not advance: %d -> %d", preCancel.Cancelled, post.Cancelled)
	}
	if post.CancelsRegistered != 0 {
		t.Errorf("cancel registry did not drain: %d", post.CancelsRegistered)
	}
}
