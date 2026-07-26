//go:build !sable_portable && windows

package main

import (
	"fmt"

	"github.com/moriyoshi/sable"
)

// fusionDemo (Windows fast build): fd fusion works here too, but with the
// completion-model contract Windows requires. Windows netpoll is IOCP, so instead
// of Go shipping readiness edges and Rust doing the read (the Unix path), **Go
// performs the overlapped read through the runtime's single IOCP and hands Rust
// the byte count**; the Rust tokio task awaits that completion. tokio still owns
// no event loop. (RustAwaitsGo's inner Go computation rides a plain Rust-side
// compute — kind 0 -> 42 — since the eventfd value channel is Unix-only.)
func fusionDemo() bool {
	got := sable.CrossingAsm(0x12345678)
	fmt.Printf("  asmcgocall fast crossing      -> %#x (want 0x12345678)\n", got)

	got2 := sable.RustAwaitsGo(0, 0)
	fmt.Printf("  tokio task awaited Go         -> %d (want 42)\n", got2)

	got3 := sable.ReadPipeViaRust(4096, 4)
	fmt.Printf("  IOCP read fused to Go netpoll -> %d bytes (want 4096)\n", got3)

	if got != 0x12345678 || got2 != 42 || got3 != 4096 {
		return false
	}
	fmt.Println("       fast crossing (asmcgocall) + mutual await via gopark/goready;")
	fmt.Println("       fd fusion via Go's single IOCP (Go reads, Rust awaits the count)")
	return true
}
