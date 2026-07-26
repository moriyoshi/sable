//go:build !sable_safe && !sable_portable

package sable

// trampoline.go — M4 deep seam #3: the rust2go-style asm fast path for the
// hottest Go->Rust crossings.
//
// Instead of the full cgocall (entersyscall -> asmcgocall -> exitsyscall), we
// call the runtime's asmcgocall DIRECTLY. asmcgocall still switches to the g0
// stack and records the switch (so traceback/GC stay correct) — we reuse Go's
// own tested switch code rather than hand-writing g0/offset asm — but we skip
// the entersyscall/exitsyscall bookkeeping: two atomic P-state transitions plus
// the exitsyscall P re-acquire. This is a SCHEDULER property, not an OS one, so
// it holds identically on every cgo target including windows/amd64.
//
// HAZARD (documented; see M5 tests): skipping entersyscall leaves the P
// _Prunning for the duration of the call, so sysmon cannot reclaim it and it
// cannot reach a GC safepoint until the call returns. This is only sound for
// crossings that are short, non-blocking, non-Go-allocating, and never re-enter
// Go. The pump's sable_fd_ready crossing (trampoline_fusion_unix.go) qualifies;
// so does the sable_noop benchmark crossing here. Set -tags sable_safe to fall
// back to the plain cgo path (trampoline_safe.go).
//
// Bodyless asmcgocall declaration is permitted by linkname_stubs.s.

// #include "sable.h"
// static void *sable_noop_addr(void) { return (void *)sable_noop; }
import "C"

import "unsafe"

//go:linkname asmcgocall runtime.asmcgocall
func asmcgocall(fn, arg unsafe.Pointer) int32

// Raw address of the Rust C-ABI noop. C.<fn> is a cgo call wrapper, not directly
// addressable, so we fetch the symbol address via a tiny C helper.
var noopAddr = unsafe.Pointer(C.sable_noop_addr())

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
