//go:build !sable_safe && !sable_portable

package sable

// CrossingAsm performs the raw Go->Rust crossing via the asmcgocall fast path
// (g0 switch, no entersyscall/exitsyscall), in isolation, for the microbenchmark
// (see bench/). It exists only in the default build, where the asm fast path is
// compiled in; the -tags sable_safe build omits it.
func CrossingAsm(x uint64) uint64 { return noopAsm(x) }
