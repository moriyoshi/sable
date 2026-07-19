//go:build sable_safe && !sable_portable

package sable

// trampoline_safe.go — the safe fallback for the hot crossing (built with
// -tags sable_safe). Uses the full, always-correct cgo path. This is the
// permanent correctness backstop: the M1–M3 suite must pass identically under
// -tags sable_safe and under the default asm fast path.

// #include "sable.h"
import "C"

//go:nosplit
func fdReady(regid uint64) {
	C.sable_fd_ready(C.uint64_t(regid))
}

func noopCgo(x uint64) uint64 {
	return uint64(C.sable_noop(C.uint64_t(x)))
}
