//go:build !sable_portable

package sable

// park.go — M3 deep seam #2: park/unpark awaiting goroutines with the runtime's
// own scheduler primitives (gopark/goready) instead of a channel.
//
// The critical safety rule (see the risk analysis): gopark/goready are legal
// ONLY on a valid Go M. Awaiting goroutines gopark themselves (legal — on-self).
// Completions are delivered by goready — but ONLY from the dispatcher goroutine
// (a real Go M woken by the netpoller), NEVER from the Rust executor thread
// (a raw pthread with no g/M). Rust merely enqueues + rings the doorbell.
//
// Concurrency detail: gopark's unlockf (parkCommit) runs on the g0 system
// stack, where sync/atomic's race-detector hooks are invalid (they crash under
// -race). So all shared-slot state is touched via the runtime-INTERNAL atomics
// (internal/runtime/atomic.*), which are //go:nosplit and carry no race hooks.
// They provide real ordering; the race detector simply doesn't see these
// accesses (they are function calls into an un-instrumented package), which is
// exactly what we want for this hand-synchronized state machine.
//
// Bodyless declarations are permitted by the presence of linkname_stubs.s.

import (
	"sync"
	"unsafe"
)

const (
	awaitIdle uint32 = iota
	awaitParked
	awaitReady
)

// gopark wait-reason labels. Cosmetic (traceback / execution trace only).
const (
	waitReasonSable uint8 = 0
	traceBlockSable uint8 = 0
)

// gopark parks the current goroutine. unlockf runs on g0 after the g is marked
// waiting but before it fully parks; returning false ABORTS the park.
//
//go:linkname gopark runtime.gopark
func gopark(unlockf func(unsafe.Pointer, unsafe.Pointer) bool, lock unsafe.Pointer, reason uint8, traceReason uint8, traceskip int)

// goready makes a parked g runnable again (and wakes a P to run it).
//
//go:linkname goready runtime.goready
func goready(gp unsafe.Pointer, traceskip int)

// Runtime-internal atomics (nosplit, no race hooks) — safe on the g0 stack.
//
//go:linkname atomicCas internal/runtime/atomic.Cas
func atomicCas(ptr *uint32, old, new uint32) bool

//go:linkname atomicXchg internal/runtime/atomic.Xchg
func atomicXchg(ptr *uint32, new uint32) uint32

//go:linkname atomicLoad internal/runtime/atomic.Load
func atomicLoad(ptr *uint32) uint32

//go:linkname atomicStore64 internal/runtime/atomic.Store64
func atomicStore64(ptr *uint64, val uint64)

//go:linkname atomicLoad64 internal/runtime/atomic.Load64
func atomicLoad64(ptr *uint64) uint64

type awaitSlot struct {
	state  uint32  // awaitIdle/Parked/Ready (via internal atomics)
	gp     uintptr // parked *g; written in parkCommit (norace), read in deliver
	result uint64  // via internal atomics; 8-aligned (offset 16)
}

const portableBuild = false

// tokenCtr lives in bridge.go (shared by both backends).
var awaits sync.Map // uint64 token -> *awaitSlot

// awaitViaPark registers a slot, runs spawn(token) to kick off the Rust work,
// then parks the current goroutine via gopark until deliverCompletion resumes
// it. No channel, no dedicated OS thread — the goroutine parks directly in Go's
// scheduler.
func awaitViaPark(spawn func(token uint64)) uint64 {
	token := tokenCtr.Add(1)
	slot := &awaitSlot{}
	awaits.Store(token, slot)
	spawn(token)
	gopark(parkCommit, unsafe.Pointer(slot), waitReasonSable, traceBlockSable, 1)
	// Resumed: either goready'd after a real park, or the park was aborted
	// because the result already landed. This acquire pairs with deliver's Xchg.
	_ = atomicLoad(&slot.state)
	return atomicLoad64(&slot.result)
}

// awaitViaParkTry is awaitViaPark for an admission that may be refused: spawn
// returns whether the task was admitted. On refusal NO completion will fire on
// the token, so we must NOT park — we drop the slot and report (0, false).
func awaitViaParkTry(spawn func(token uint64) bool) (uint64, bool) {
	token := tokenCtr.Add(1)
	slot := &awaitSlot{}
	awaits.Store(token, slot)
	if !spawn(token) {
		awaits.Delete(token)
		return 0, false
	}
	gopark(parkCommit, unsafe.Pointer(slot), waitReasonSable, traceBlockSable, 1)
	_ = atomicLoad(&slot.state)
	return atomicLoad64(&slot.result), true
}

// parkCommit is gopark's unlockf. It stashes the g pointer and commits the park
// only if the result has not already been delivered — the lost-wakeup guard.
// Runs on g0: //go:norace + //go:nosplit + internal atomics + a bare-uintptr gp
// store (no write barrier; a g is never GC-moved and stays alive while parked).
//
//go:norace
//go:nosplit
//go:nocheckptr
func parkCommit(gp unsafe.Pointer, slotPtr unsafe.Pointer) bool {
	slot := (*awaitSlot)(slotPtr)
	slot.gp = uintptr(gp)
	return atomicCas(&slot.state, awaitIdle, awaitParked)
}

// deliverCompletion delivers a result and wakes the awaiting goroutine. MUST be
// called only from the dispatcher goroutine (a valid Go M). //go:nocheckptr
// permits the uintptr->*g conversion (the g is valid runtime memory, never
// GC-moved, kept alive while parked).
//
//go:nocheckptr
func deliverCompletion(token uint64, result uint64) {
	v, ok := awaits.LoadAndDelete(token)
	if !ok {
		return
	}
	slot := v.(*awaitSlot)
	atomicStore64(&slot.result, result)
	// If the goroutine already parked, wake it; if not (still Idle), parkCommit
	// will observe Ready and abort its park instead.
	if atomicXchg(&slot.state, awaitReady) == awaitParked {
		goready(unsafe.Pointer(slot.gp), 1)
	}
}
