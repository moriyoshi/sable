//go:build !sable_safe && !sable_portable && unix

package sable

// trampoline_fusion_unix.go — the pump's hot Go->Rust crossing (sable_fd_ready,
// fired on every readiness edge), split out of trampoline.go. It rides the same
// asmcgocall fast path (declared there), but references the Rust `sable_fd_ready`
// export, which only exists in the Unix fast build (fd fusion is readiness-based;
// see poll.go / rust/src/reactor.rs). The Windows fast build has no pump, so this
// crossing is compiled out with it.
//
// Bodyless asmcgocall use is permitted by linkname_stubs.s.

// #include "sable.h"
// static void *sable_fd_ready_addr(void) { return (void *)sable_fd_ready; }
import "C"

import "unsafe"

var fdReadyAddr = unsafe.Pointer(C.sable_fd_ready_addr())

// fdReady is the hot crossing used by every pump readiness event. The regid is
// passed as asmcgocall's `arg`, which lands in the C ABI's first argument
// register (sable_fd_ready(uint64_t regid)). It is a small integer, never a real
// heap pointer, so the GC ignores it when scanning.
//
//go:nosplit
//go:nocheckptr
func fdReady(regid uint64) {
	asmcgocall(fdReadyAddr, unsafe.Pointer(uintptr(regid)))
}
