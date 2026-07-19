//go:build sable_http

// Command http demonstrates the fusion with real tokio-ecosystem apps: an axum
// server + a reqwest client on a shared-completion io-enabled runtime. Each HTTP
// request is driven by a Go goroutine that parks via gopark and is resumed via
// goready when the response arrives; the /go handler calls back into Go, so the
// path crosses Go -> reqwest -> axum -> Go and back. See examples/rust2go.md's
// sibling scenario and README. Build/run via `make run-http`/`make test-http`.
package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/moriyoshi/sable"
)

func main() {
	sable.Init()
	httpInit()

	port := startAxum(0)
	if port == 0 {
		fmt.Println("[http] axum failed to bind")
		return
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	for i := 0; i < 100 && awaitHttpGet(base+"/ping") != 7; i++ {
		time.Sleep(20 * time.Millisecond)
	}

	fmt.Printf("[http] axum listening on 127.0.0.1:%d (separate io-enabled tokio runtime)\n", port)
	fmt.Printf("[http] goroutine → reqwest GET /ping    → %d (want 7)\n", awaitHttpGet(base+"/ping"))
	fmt.Printf("[http] goroutine → reqwest GET /go?n=21 → %d (want 42; axum handler called back into Go)\n", awaitHttpGet(base+"/go?n=21"))

	const n = 200
	var wg sync.WaitGroup
	var bad int
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(k uint64) {
			defer wg.Done()
			if awaitHttpGet(fmt.Sprintf("%s/go?n=%d", base, k)) != 2*k {
				mu.Lock()
				bad++
				mu.Unlock()
			}
		}(uint64(i))
	}
	wg.Wait()
	fmt.Printf("[http] %d concurrent goroutines drove reqwest→axum→Go, %d wrong\n", n, bad)
	fmt.Println("[http] OK — reqwest + axum fused with Go's scheduler via gopark/goready")

	sable.Shutdown()
}
