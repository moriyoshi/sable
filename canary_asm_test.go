//go:build !sable_safe && !sable_portable

package sable

import "testing"

// runtime.asmcgocall — the fast-path crossing. Verifies the linkname resolves
// and the g0-stack switch + arg passing still work (only in the default,
// non-sable_safe build, where the asm path exists).
func TestCanaryAsmcgocall(t *testing.T) {
	const x = 0x12345678 // < 2^32: asmcgocall returns the callee's result as int32
	if got := noopAsm(x); got != x {
		t.Fatalf("asmcgocall crossing = %#x, want %#x", got, x)
	}
}
