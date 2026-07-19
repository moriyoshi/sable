//go:build sable_http

package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestReqwestAxumFusion exercises the fusion with real tokio ecosystem apps:
// an axum server + a reqwest client on a shared-completion io-enabled runtime.
// Each HTTP request is driven by a Go goroutine that parks via gopark and is
// resumed via goready when the response arrives — and the /go handler calls back
// into Go, so the request path crosses Go -> reqwest -> axum -> Go and back.
func TestReqwestAxumFusion(t *testing.T) {
	httpInit()

	port := startAxum(0) // OS-assigned free port
	if port == 0 {
		t.Fatal("axum failed to bind")
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	// Wait for the server to be accepting.
	ready := false
	for i := 0; i < 100; i++ {
		if awaitHttpGet(base+"/ping") == 7 {
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("axum /ping never returned 7")
	}

	// Bidirectional round trip: Go -> reqwest -> axum handler -> Go(double) -> back.
	if got := awaitHttpGet(base + "/go?n=21"); got != 42 {
		t.Fatalf("/go?n=21 = %d, want 42", got)
	}

	// Concurrent load through the fusion: many goroutines each drive a reqwest
	// GET, parking in Go's scheduler until goready delivers the result.
	const n = 200
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := uint64(i)
			if got := awaitHttpGet(fmt.Sprintf("%s/go?n=%d", base, k)); got != 2*k {
				errs <- fmt.Sprintf("n=%d -> %d, want %d", k, got, 2*k)
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
		t.Fatalf("%d/%d concurrent HTTP awaits wrong", count, n)
	}
}

func benchHTTPBase(b *testing.B) string {
	httpInit()
	port := startAxum(0)
	if port == 0 {
		b.Fatal("axum failed to bind")
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	for i := 0; i < 100 && awaitHttpGet(base+"/ping") != 7; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	return base
}

// BenchmarkHTTPPing — HTTP req/sec through the fusion (Go goroutine -> reqwest ->
// axum -> gopark/goready), no Go callback in the path.
func BenchmarkHTTPPing(b *testing.B) {
	base := benchHTTPBase(b)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if awaitHttpGet(base+"/ping") != 7 {
				b.Fatal("bad /ping")
			}
		}
	})
}

// BenchmarkHTTPGoCallback — full bidirectional path per request: Go -> reqwest ->
// axum handler -> Go(double) -> back.
func BenchmarkHTTPGoCallback(b *testing.B) {
	base := benchHTTPBase(b)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = awaitHttpGet(base + "/go?n=21")
		}
	})
}
