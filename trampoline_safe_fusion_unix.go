//go:build sable_safe && !sable_portable && unix

package sable

// trampoline_safe_fusion_unix.go — the pump's fd_ready crossing on the safe cgo
// path, split out of trampoline_safe.go for the same reason as the asm variant
// (trampoline_fusion_unix.go): the Rust `sable_fd_ready` export exists only in the
// Unix fast build, so this compiles out of the Windows fast build along with the
// pump.

// #include "sable.h"
import "C"

//go:nosplit
func fdReady(regid uint64) {
	C.sable_fd_ready(C.uint64_t(regid))
}
