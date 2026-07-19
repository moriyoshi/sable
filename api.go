package sable

// api.go — the public, importable API surface of the sable fusion runtime.
//
// Example programs, the demo binary, and the microbenchmarks live OUTSIDE this
// package (see cmd/sable-demo, examples/, and bench/) and reach the runtime
// through the functions here rather than through the internal helpers. Keeping
// that boundary explicit is what makes sable publishable as a library instead
// of a single demo binary.

import (
	"sync"
	"unsafe"
)

var initOnce sync.Once

// Init builds the fused runtime and starts the completion dispatcher. It is
// idempotent and safe to call concurrently; every public entry point also calls
// it lazily, so most programs never need to call Init explicitly. Init panics
// (fails closed) on an uncertified Go toolchain unless SABLE_ALLOW_UNVERIFIED_GO
// is set — see Verified and guard.go.
func Init() { initOnce.Do(sableInit) }

// AwaitRust: a goroutine awaits a built-in Rust async future (kind 0 -> sleep
// 1ms, return 42; kind 1 -> echo arg). The goroutine parks in Go's scheduler; no
// OS thread is pinned while the tokio task runs on the fused runtime. Available
// in every build (the core Go-awaits-Rust direction).
func AwaitRust(kind uint32, arg uint64) uint64 {
	Init()
	return awaitRust(kind, arg)
}

// AwaitToken parks the calling goroutine on a fresh completion token, runs spawn
// (which must kick off a Rust task tagged with that token via the FFI), and
// returns the raw u64 result delivered by the runtime's dispatcher. No OS thread
// is blocked while the task runs. This is the low-level primitive behind Call,
// and what an advanced example uses when its Rust result is a bare u64 (e.g. the
// http example's response length).
func AwaitToken(spawn func(token uint64)) uint64 {
	Init()
	return awaitViaPark(spawn)
}

// RuntimePtr returns the raw address of the shared SableRuntime, for advanced
// FFI that must hand the runtime to Rust directly — e.g. the real rust2go g2r
// binding (which takes the runtime as a u64) or the http runtime constructor.
// The pointer is owned by the library; never free it (use Shutdown instead).
func RuntimePtr() uintptr {
	Init()
	return uintptr(unsafe.Pointer(rt))
}
