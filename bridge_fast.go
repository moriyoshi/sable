//go:build !sable_portable

package sable

// bridge_fast.go — reactor/pump-INDEPENDENT fast-path plumbing that the portable
// (zero-linkname) build excludes but every non-portable target keeps, Windows
// included. The fd-fusion pieces (a Rust tokio task reading a pipe whose readiness
// comes from Go's netpoll, and the eventfd value channel behind the "Rust awaits
// Go" compute demo) are readiness-based and live in bridge_fusion_unix.go.

// #include "sable.h"
import "C"

// demoRustAwaitsGo runs the single-shot "tokio task awaits Go" demo.
func demoRustAwaitsGo(kind uint32, arg uint64) uint64 {
	return awaitRustAwaitsGo(kind, arg)
}

// awaitRustAwaitsGo: a goroutine awaits a Rust task that itself awaits Go. On
// Unix the Rust task awaits a real Go computation over the eventfd value channel;
// on Windows the Rust side falls back to a plain compute (the value channel is
// Unix-only), but the token still completes, so the await stays uniform. Either
// way delivery rides the completion doorbell/dispatcher — no fd fusion.
func awaitRustAwaitsGo(kind uint32, arg uint64) uint64 {
	return awaitViaPark(func(token uint64) {
		C.sable_spawn_rust_awaits_go(rt, C.uint32_t(kind), C.uint64_t(arg), C.uint64_t(token))
	})
}
