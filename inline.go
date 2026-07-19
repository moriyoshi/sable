//go:build !sable_portable

package sable

// inline.go — M7 inline fast path with a Pending fallback.
//
// A Go goroutine polls a Rust future ON ITS OWN M. If the future is Ready, the
// result is returned with no spawn, no gopark, no cross-thread wakeup — the
// common (non-suspending) case runs at FFI-crossing speed. Only if the future
// returns Pending does it fall back to the async park/deliver path (gopark +
// the doorbell/dispatcher goready of the earlier milestones), and that path is
// only taken by genuinely-suspending awaits, where the I/O cost dominates.

// #include "sable.h"
import "C"

import "unsafe"

// awaitInlineOneshot is the pure fast-path floor (create+poll+drop in one
// crossing); it cannot fall back, so it's for compute futures / benchmarking.
func awaitInlineOneshot(kind uint32, arg uint64) uint64 {
	return uint64(C.sable_await_inline(C.uint32_t(kind), C.uint64_t(arg)))
}

// awaitInline is the full M7 path: inline poll, with a Pending fallback that
// parks the goroutine and re-polls when the future's waker fires.
func awaitInline(kind uint32, arg uint64) uint64 {
	fut := C.sable_future_new(C.uint32_t(kind), C.uint64_t(arg))
	defer C.sable_future_drop(fut)

	// Fast path: one poll with a no-op waker (no alloc, no map, no park).
	var out C.uint64_t
	if C.sable_future_poll_noop(fut, &out) == 1 {
		return uint64(out)
	}

	// Pending fallback: install a real waker keyed by a token, park until it
	// fires (delivered via the doorbell/dispatcher as a goready), then re-poll.
	for {
		token := tokenCtr.Add(1)
		slot := &awaitSlot{}
		awaits.Store(token, slot)
		if C.sable_future_poll(rt, fut, C.uint64_t(token), &out) == 1 {
			awaits.Delete(token)
			return uint64(out)
		}
		// parkCommit aborts the park if the waker already fired (lost-wakeup safe).
		gopark(parkCommit, unsafe.Pointer(slot), waitReasonSable, traceBlockSable, 1)
		_ = atomicLoad(&slot.state) // acquire; pairs with deliver's release
	}
}
