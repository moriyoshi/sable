//go:build !sable_portable

package sable

// goexec.go — the Go side of the M-driven executor. One worker goroutine per P,
// each parked in Go's netpoller on its OWN per-worker eventfd doorbell (so
// wakeups go through Go's scheduler, the primitive it is tuned for). tokio-style
// tasks are thus polled across Go's M:N scheduler instead of on one dedicated
// tokio thread. A completing task calls back (sable_go_deliver) on its worker's
// M, delivering the result with a direct goready — no doorbell/dispatcher hop.

// #include "sable.h"
import "C"

import (
	"os"
	"runtime"
	"sync"
)

var goexecOnce sync.Once

func goexecInit() {
	goexecOnce.Do(func() {
		n := runtime.GOMAXPROCS(0)
		C.sable_goexec_init(C.int(n))
		for i := 0; i < n; i++ {
			efd := int(C.sable_goexec_worker_efd(C.int(i)))
			// os.NewFile registers the (nonblocking) doorbell with the netpoller,
			// so Read parks the worker goroutine instead of blocking an OS thread.
			f := os.NewFile(uintptr(efd), "goexec-worker")
			if f == nil {
				panic("os.NewFile(goexec doorbell) failed")
			}
			go goexecWorker(C.int(i), f)
		}
	})
}

func goexecWorker(i C.int, f *os.File) {
	buf := make([]byte, 8)
	for {
		if _, err := f.Read(buf); err != nil { // park in netpoll on this worker's doorbell
			return
		}
		C.sable_goexec_run_worker(i) // drain+run this worker's queue on this M
	}
}

// awaitGoExec: a goroutine awaits a task executed on Go's Ms (goexec), parking
// via gopark until a worker delivers the result via goready.
func awaitGoExec(kind uint32, arg uint64) uint64 {
	goexecInit()
	return awaitViaPark(func(token uint64) {
		C.sable_goexec_spawn(C.uint32_t(kind), C.uint64_t(arg), C.uint64_t(token))
	})
}

//export sable_go_deliver
func sable_go_deliver(token C.uint64_t, result C.uint64_t) {
	// Runs on a worker goroutine's M (cgo callback from within run_worker), so
	// the goready inside deliverCompletion is legal and direct.
	deliverCompletion(uint64(token), uint64(result))
}
