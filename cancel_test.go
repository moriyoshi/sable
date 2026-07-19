package sable

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestCancel: cancelling ctx aborts an in-flight 10s Rust op and returns
// promptly with ctx.Err(), and the cancellable-call registry drains to 0.
func TestCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	start := time.Now()
	go func() {
		_, err := CallCtx(ctx, OpSleepLong, []byte("x"))
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
		if el := time.Since(start); el > 3*time.Second {
			t.Errorf("cancel took %v (op is 10s) — not aborted", el)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CallCtx did not return after cancel — op not aborted")
	}

	// A non-cancelled CallCtx still returns its result.
	if got, err := CallCtx(context.Background(), OpEcho, []byte("ok")); err != nil || !bytes.Equal(got, []byte("ok")) {
		t.Errorf("uncancelled CallCtx: got %q err %v", got, err)
	}

	// Many concurrent cancellations, then the registry must drain to 0.
	var wg sync.WaitGroup
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cf := context.WithTimeout(context.Background(), 5*time.Millisecond)
			defer cf()
			_, _ = CallCtx(c, OpSleepLong, []byte("y"))
		}()
	}
	wg.Wait()
	// Give aborts a moment to settle (guard drop removes the entry).
	for i := 0; i < 100 && callPending() != 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if p := callPending(); p != 0 {
		t.Errorf("cancellable-call registry leak: %d pending", p)
	}
}
