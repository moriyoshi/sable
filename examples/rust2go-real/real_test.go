//go:build sable_rust2go

package main

import (
	"strings"
	"sync"
	"testing"
)

// TestRust2GoReal drives the REAL rust2go g2r binding end to end: a typed request
// crosses via rust2go's cgo shim to kick off an async task on sable's fused
// runtime, whose completion resumes the parked goroutine with the typed result.
// Run via `make test-rust2go` (which sets GODEBUG=cgocheck=0,invalidptr=0 as
// rust2go requires).
func TestRust2GoReal(t *testing.T) {
	inStock, err := CheckStockReal("widgets", 5)
	if err != nil {
		t.Fatalf("CheckStockReal(in stock): %v", err)
	}
	if !inStock.OK || !strings.Contains(inStock.Message, "reserved") {
		t.Errorf("in stock: got %+v", inStock)
	}

	outStock, err := CheckStockReal("gizmos", 200)
	if err != nil {
		t.Fatalf("CheckStockReal(out of stock): %v", err)
	}
	if outStock.OK || !strings.Contains(outStock.Message, "20") {
		t.Errorf("out of stock: got %+v", outStock)
	}

	// Concurrency: many goroutines each awaiting the async rust2go->sable call.
	const N = 200
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		qty := uint8(10 + i%40)
		wg.Add(1)
		go func(qty uint8) {
			defer wg.Done()
			r, err := CheckStockReal("u", qty)
			if err != nil {
				t.Errorf("concurrent CheckStockReal: %v", err)
				return
			}
			if r.OK != (qty <= 20) {
				t.Errorf("qty %d: ok=%v", qty, r.OK)
			}
		}(qty)
	}
	wg.Wait()
}
