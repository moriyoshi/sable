//go:build !sable_safe && !sable_portable

package sable

// trampoline.go — M4 deep seam #3: the rust2go-style asm fast path for the
// hottest Go->Rust crossing (sable_fd_ready, fired on every readiness event).
//
// Instead of the full cgocall (entersyscall -> asmcgocall -> exitsyscall), we
// call the runtime's asmcgocall DIRECTLY. asmcgocall still switches to the g0
// stack and records the switch (so traceback/GC stay correct) — we reuse Go's
// own tested switch code rather than hand-writing g0/offset asm — but we skip
// the entersyscall/exitsyscall bookkeeping: two atomic P-state transitions plus
// the exitsyscall P re-acquire.
//
// HAZARD (documented; see M5 tests): skipping entersyscall leaves the P
// _Prunning for the duration of the call, so sysmon cannot reclaim it and it
// cannot reach a GC safepoint until the call returns. This is only sound for
// crossings that are short, non-blocking, non-Go-allocating, and never re-enter
// Go. sable_fd_ready qualifies: a slab lookup + a waker fire. Set -tags
// sable_safe to fall back to the plain cgo path (trampoline_safe.go).
//
// Bodyless asmcgocall declaration is permitted by linkname_stubs.s.

// #include "sable.h"
// static void *sable_noop_addr(void)     { return (void *)sable_noop; }
// static void *sable_fd_ready_addr(void) { return (void *)sable_fd_ready; }
import "C"

import "unsafe"

//go:linkname asmcgocall runtime.asmcgocall
func asmcgocall(fn, arg unsafe.Pointer) int32

// Raw addresses of the Rust C-ABI functions. C.<fn> is a cgo call wrapper, not
// directly addressable, so we fetch the symbol address via a tiny C helper.
var (
	noopAddr    = unsafe.Pointer(C.sable_noop_addr())
	fdReadyAddr = unsafe.Pointer(C.sable_fd_ready_addr())
)

// fdReady is the hot crossing used by every pump readiness event. The regid is
// passed as asmcgocall's `arg`, which lands in x0 = the C ABI's first argument
// (sable_fd_ready(uint64_t regid)). It is a small integer, never a real heap
// pointer, so the GC ignores it when scanning.
//
//go:nosplit
//go:nocheckptr
func fdReady(regid uint64) {
	asmcgocall(fdReadyAddr, unsafe.Pointer(uintptr(regid)))
}

// noopAsm / noopCgo: the raw crossing in isolation, for the microbenchmark.
// asmcgocall returns the callee's x0 truncated to int32; the value is irrelevant
// for a benchmark sink.
//
//go:nosplit
//go:nocheckptr
func noopAsm(x uint64) uint64 {
	return uint64(uint32(asmcgocall(noopAddr, unsafe.Pointer(uintptr(x)))))
}

func noopCgo(x uint64) uint64 {
	return uint64(C.sable_noop(C.uint64_t(x)))
}
