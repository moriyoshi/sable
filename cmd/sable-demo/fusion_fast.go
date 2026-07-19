//go:build !sable_portable

package main

import (
	"fmt"

	"github.com/moriyoshi/sable"
)

// fusionDemo runs the reactor/pump single-epoll demonstration (default build):
// the reverse await direction and real fd I/O, then asserts the single-shared-
// epoll invariant.
func fusionDemo() bool {
	got2 := sable.RustAwaitsGo(0, 0)
	fmt.Printf("  tokio task awaited Go         -> %d (want 7)\n", got2)

	got3 := sable.ReadPipeViaRust(4096, 4)
	fmt.Printf("  tokio read pipe via netpoll   -> %d bytes (want 4096)\n", got3)

	nEpoll := sable.CountEpollFds()
	fmt.Printf("  epoll fds in process          -> %d (want 1: Go's netpoll)\n", nEpoll)

	if got2 != 7 || got3 != 4096 || nEpoll != 1 {
		return false
	}
	fmt.Println("       tokio fused onto Go's netpoll (single epoll); mutual await via")
	fmt.Println("       gopark/goready; hot crossing via asmcgocall")
	return true
}
