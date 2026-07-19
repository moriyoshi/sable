//go:build sable_http

package main

// http.go — Go bridge for the reqwest+axum scenario. Built only with
// `-tags sable_http`, against a Rust lib built with `--features http`.
//
// The http runtime shares the sable runtime's completion plumbing, so reqwest
// results are delivered by the SAME dispatcher + gopark/goready path as the core
// awaits — a Go goroutine drives a real HTTP request and parks in Go's scheduler
// until it resolves. It reaches the shared runtime through sable's public API
// (RuntimePtr to construct the http runtime, AwaitToken to park on a GET), and
// calls the http-specific FFI through this package's own cgo.

// #cgo CFLAGS: -I${SRCDIR}/../../include
// #include <stdlib.h>
// #include "sable.h"
import "C"

import (
	"sync"
	"unsafe"

	"github.com/moriyoshi/sable"
)

var (
	httpRt   *C.SableHttpRuntime
	httpOnce sync.Once
)

func httpInit() {
	httpOnce.Do(func() {
		// RuntimePtr initializes sable if needed and returns the shared runtime.
		rt := (*C.SableRuntime)(unsafe.Pointer(sable.RuntimePtr()))
		httpRt = C.sable_http_new(rt)
		if httpRt == nil {
			panic("sable_http_new failed")
		}
	})
}

// startAxum starts the axum server (port 0 = OS-assigned) and returns the bound
// port (0 on error).
func startAxum(port uint16) uint16 {
	return uint16(C.sable_http_start_axum(httpRt, C.uint16_t(port)))
}

// awaitHttpGet: a goroutine drives a reqwest GET and parks (gopark) until the
// response body (parsed as u64) is delivered via the shared dispatcher.
func awaitHttpGet(url string) uint64 {
	return sable.AwaitToken(func(token uint64) {
		curl := C.CString(url)
		defer C.free(unsafe.Pointer(curl))
		C.sable_http_spawn_get(httpRt, curl, C.size_t(len(url)), C.uint64_t(token))
	})
}

//export sable_go_double
func sable_go_double(n C.uint64_t) C.uint64_t {
	// Called from an axum handler on a tokio worker thread (Rust -> Go via
	// needm). Demonstrates the reverse direction of the fusion inside a real
	// request path.
	return 2 * n
}
