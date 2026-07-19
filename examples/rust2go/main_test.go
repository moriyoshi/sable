package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moriyoshi/sable"
)

// TestRust2GoStyle exercises the rust2go-style typed async binding: typed
// request in, async Rust handler, typed response out — the stock-check use case.
func TestRust2GoStyle(t *testing.T) {
	inStock, err := CheckStock(context.Background(), Order{Item: "widgets", Qty: 5})
	if err != nil {
		t.Fatalf("CheckStock(in stock): %v", err)
	}
	if !inStock.OK || !strings.Contains(inStock.Message, "reserved") {
		t.Errorf("in stock: got %+v, want ok + reserved", inStock)
	}

	outStock, err := CheckStock(context.Background(), Order{Item: "gizmos", Qty: 200})
	if err != nil {
		t.Fatalf("CheckStock(out of stock): %v", err)
	}
	if outStock.OK || !strings.Contains(outStock.Message, "20") {
		t.Errorf("out of stock: got %+v, want !ok + stock-level message", outStock)
	}

	// The rust2go value proposition: many concurrent goroutines each awaiting an
	// async Rust call, none of them pinning an OS thread.
	const N = 500
	var wg sync.WaitGroup
	var mu sync.Mutex
	filled := 0
	for i := 0; i < N; i++ {
		qty := uint8(10 + i%40) // straddles the 20-in-stock boundary
		wg.Add(1)
		go func(qty uint8) {
			defer wg.Done()
			r, err := CheckStock(context.Background(), Order{Item: "u", Qty: qty})
			if err != nil {
				t.Errorf("concurrent CheckStock: %v", err)
				return
			}
			if r.OK != (qty <= 20) {
				t.Errorf("qty %d: ok=%v", qty, r.OK)
			}
			if r.OK {
				mu.Lock()
				filled++
				mu.Unlock()
			}
		}(qty)
	}
	wg.Wait()
	if filled == 0 || filled == N {
		t.Errorf("expected a mix of fulfilled/refused, got %d/%d", filled, N)
	}
}

// TestRust2GoStyleCancel shows the cancellation parity: cancelling ctx aborts the
// in-flight Rust task (the CheckStock goroutine returns promptly with ctx.Err()).
func TestRust2GoStyleCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	// Reuse the long-sleep op through the raw Call path to model a slow lookup.
	start := time.Now()
	_, err := sable.CallCtx(ctx, opSleepLong, encodeOrder(Order{Item: "bulk", Qty: 30}))
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("cancel took %v (op is 10s) — not aborted", el)
	}
}
