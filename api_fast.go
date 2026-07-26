//go:build !sable_portable

package sable

// api_fast.go — public API available only in the deep-integration (non-portable)
// build, and OS-neutral within it (Windows included). The fd-fusion demonstrations
// (ReadPipeViaRust, CountEpollFds) are Unix-only and live in api_fusion_unix.go.
// (AwaitRust lives in api.go because the core Go-awaits-Rust path exists in the
// portable build too.)

// RustAwaitsGo runs the reverse direction: a goroutine awaits a Rust tokio task
// that itself awaits a Go computation, demonstrating symmetric mutual await. On
// Unix the inner await rides the eventfd value channel over the shared epoll
// (kind 0 -> 7); on Windows that value channel is unavailable, so the Rust side
// computes plainly — the await still completes uniformly.
func RustAwaitsGo(kind uint32, arg uint64) uint64 {
	Init()
	return demoRustAwaitsGo(kind, arg)
}

// CrossingCgo performs the raw Go->Rust crossing via the full cgo path, in
// isolation, for the microbenchmark (see bench/).
func CrossingCgo(x uint64) uint64 { return noopCgo(x) }
