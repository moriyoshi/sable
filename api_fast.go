//go:build !sable_portable

package sable

// api_fast.go — public API available only in the deep-integration (non-portable)
// build: the mutual-await demonstrations the test suite validates, surfaced for
// the demo binary and benchmarks. The portable build compiles these out.
// (AwaitRust lives in api.go because the core Go-awaits-Rust path exists in the
// portable build too.)

// RustAwaitsGo runs the reverse direction: a goroutine awaits a Rust tokio task
// that itself awaits a Go computation (kind 0 -> 7), demonstrating symmetric
// mutual await over the single shared epoll.
func RustAwaitsGo(kind uint32, arg uint64) uint64 {
	Init()
	return demoRustAwaitsGo(kind, arg)
}

// ReadPipeViaRust has a Rust tokio task read nbytes (written in chunks) from a
// pipe whose readiness is sourced entirely from Go's netpoll, returning the
// bytes read. It demonstrates real fd I/O flowing through the one shared epoll.
func ReadPipeViaRust(nbytes, chunks int) uint64 {
	Init()
	return readPipeViaRust(nbytes, chunks)
}

// CountEpollFds reports how many epoll instances the process holds. With the
// runtime initialized this is 1 (Go's netpoll) — the single-shared-epoll
// invariant, tokio owning none.
func CountEpollFds() int { return countEpollFds() }

// CrossingCgo performs the raw Go->Rust crossing via the full cgo path, in
// isolation, for the microbenchmark (see bench/).
func CrossingCgo(x uint64) uint64 { return noopCgo(x) }
