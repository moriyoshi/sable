//go:build !sable_portable

package sable

// poll.go — the Go side of the single-shared-epoll bridge. For each fused fd,
// Rust starts a "pump" goroutine here; the pump registers the fd with Go's
// netpoller and parks in poll_runtime_pollWait. On a readable edge it calls back
// into Rust (fdReady -> sable_fd_ready), which fires the tokio Waker.
//
// Lifecycle (leak-free, UAF-free):
//   * The pump is the SOLE owner of the pollDesc. It loops pollWait->fdReady;
//     edge-triggered netpoll means an undrained fd yields no new edge, so the
//     pump simply parks — no busy loop, no explicit "arm" handshake needed.
//   * sable_go_poll_stop wakes a pump parked in pollWait via pollUnblock (which
//     also sets pd.closing, required before pollClose). pollUnblock returns
//     before the pump's pollWait returns, so the pump's own pollClose can never
//     race ahead of it. A CAS on `pd` resolves the stop-before-open race.

// #include "sable.h"
import "C"

import (
	"sync"
	"sync/atomic"
	"syscall"
)

const (
	pollModeRead = int('r')
	pollNoError  = 0
)

// pdStopped is a sentinel stored in pumpReg.pd when a stop arrives before the
// pump has opened its pollDesc.
const pdStopped = ^uintptr(0)

type pumpReg struct {
	pd atomic.Uintptr // 0 = not yet opened; pdStopped = stop-before-open; else the pollDesc
}

var pumps sync.Map // uint64 regid -> *pumpReg

//export sable_go_poll_start
func sable_go_poll_start(fd C.uintptr_t, regid C.uint64_t) {
	reg := &pumpReg{}
	pumps.Store(uint64(regid), reg)
	go pump(uintptr(fd), uint64(regid), reg)
}

//export sable_go_poll_stop
func sable_go_poll_stop(regid C.uint64_t) {
	v, ok := pumps.LoadAndDelete(uint64(regid))
	if !ok {
		return
	}
	reg := v.(*pumpReg)
	// If the pump hasn't opened its pd yet, claim the stop-before-open slot; the
	// pump will observe pdStopped after pollOpen and tear down itself.
	if reg.pd.CompareAndSwap(0, pdStopped) {
		return
	}
	// The pump owns a real pd. Wake it out of pollWait; it exits and pollCloses.
	// pollUnblock returns before pollWait can return, so no use-after-free.
	poll_runtime_pollUnblock(reg.pd.Load())
}

func pump(fd uintptr, regid uint64, reg *pumpReg) {
	// The pump is the SOLE owner of the fused fd and closes it LAST — after any
	// pollClose (epoll DEL). This ordering is essential: if the fd were closed
	// before its netpoll registration is removed, the OS could reuse the fd
	// number for another task's fd, and this pump's later pollClose would DEL
	// the WRONG registration, silently starving that task of edges (a hang).
	defer syscall.Close(int(fd))

	pd, errno := poll_runtime_pollOpen(fd)
	if errno != 0 {
		// Not pollable: signal readiness so the Rust side proceeds (its read
		// will surface the real error) rather than hanging.
		fdReady(regid)
		return
	}
	// Publish pd, unless a stop already arrived (reg.pd == pdStopped). In that
	// stop-before-open case, poll_stop did NOT touch pd, so we own the full
	// teardown (pollUnblock sets pd.closing, required before pollClose).
	if !reg.pd.CompareAndSwap(0, pd) {
		poll_runtime_pollUnblock(pd)
		poll_runtime_pollClose(pd)
		return
	}

	// Optimistic INITIAL readiness. A write that happened before pollOpen
	// produced no epoll edge (the fd wasn't registered yet), and with EPOLLET no
	// later edge re-reports unread data — so force the Rust side to attempt a
	// read AFTER registration. If data is present it is consumed; if not, the
	// read returns EAGAIN and Rust waits for a real edge. (We also do NOT call
	// pollReset: it would store pdNil and erase a pdReady an edge latched.)
	fdReady(regid)

	for {
		// netpollblock consumes a pending pdReady if an edge arrived since the
		// last wait, else parks in Go's netpoll — no OS thread pinned, no spin.
		if poll_runtime_pollWait(pd, pollModeRead) != pollNoError {
			// Closing: poll_stop already called pollUnblock exactly once, so we
			// only pollClose here (pollUnblock is NOT idempotent — a second call
			// throws "unblock on closing polldesc").
			poll_runtime_pollClose(pd)
			return
		}
		fdReady(regid)
	}
}
