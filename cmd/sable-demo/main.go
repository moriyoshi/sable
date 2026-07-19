// Command sable-demo is the runnable demonstration of the sable fusion runtime:
// a goroutine awaits an async Rust result, tokio awaits a Go result, and real fd
// I/O flows through the single shared epoll — all over the library's public API.
//
// The rust2go and reqwest+axum scenarios are their own programs under examples/.
package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/moriyoshi/sable"
)

func main() {
	sable.Init() // builds the fused runtime; fails closed on an uncertified Go

	fmt.Printf("sable demo [go=%s %s/%s, abi-verified=%t]\n",
		runtime.Version(), runtime.GOOS, runtime.GOARCH, sable.Verified())

	// Core (every build): a goroutine awaits a Rust async result.
	got := sable.AwaitRust(0, 0)
	fmt.Printf("  goroutine awaited Rust        -> %d (want 42)\n", got)

	// The reactor/pump single-epoll demonstration is build-specific.
	ok := fusionDemo()

	if got != 42 || !ok {
		fmt.Fprintln(os.Stderr, "sable: FAILED")
		os.Exit(1)
	}
	fmt.Println("sable: OK")

	// Graceful teardown: abort in-flight tasks, stop the dispatcher, join the
	// executor thread, close the eventfd. Not required for a process about to
	// exit, but exercises the production shutdown path on every run.
	sable.Shutdown()
}
