//go:build !sable_portable && windows

package main

import (
	"fmt"

	"github.com/moriyoshi/sable"
)

// fusionDemo (Windows fast build): fd fusion — a foreign fd whose readiness comes
// from Go's netpoll — is Unix-only, because Windows netpoll is IOCP (completion-
// based), not readiness-based. So the single-shared-epoll pump demo is omitted
// here. What remains is the OS-neutral fast path: the raw asmcgocall crossing and
// mutual await via gopark/goready. RustAwaitsGo still runs, but its inner Go
// computation rides a plain Rust-side compute (kind 0 -> 42) rather than the
// Unix-only eventfd value channel.
func fusionDemo() bool {
	got := sable.CrossingAsm(0x12345678)
	fmt.Printf("  asmcgocall fast crossing      -> %#x (want 0x12345678)\n", got)

	got2 := sable.RustAwaitsGo(0, 0)
	fmt.Printf("  tokio task awaited Go         -> %d (want 42; fd fusion omitted)\n", got2)

	if got != 0x12345678 || got2 != 42 {
		return false
	}
	fmt.Println("       fast crossing (asmcgocall) + mutual await via gopark/goready;")
	fmt.Println("       fd fusion (single-epoll pump) omitted — Windows netpoll is IOCP")
	return true
}
