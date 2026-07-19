package sable

import "unsafe"

// Architecture support.
//
// Sable is arch-neutral across 64-bit targets: the M4 fast path calls
// runtime.asmcgocall (which exists on every cgo arch and sets up the C ABI
// itself — arg -> x0 on arm64, DI on amd64, etc.), and every //go:linkname
// target (gopark/goready, poll_runtime_poll*, internal/runtime/atomic.*) is a
// Go-source-level symbol the toolchain resolves per-arch. There is no
// hand-written, arch-specific assembly to port. A NATIVE build on amd64,
// arm64, riscv64, ppc64le, s390x, loong64, ... just works.
//
// 32-bit arches are NOT supported: fdReady passes the u64 regid to asmcgocall as
// a pointer-sized argument (a 4-byte uintptr would truncate it), and
// awaitSlot.result uses 64-bit atomics that require 8-byte alignment (only the
// first heap word is guaranteed 8-aligned on 32-bit). The line below is a
// compile-time assertion that uintptr is 8 bytes; on a 32-bit arch it fails to
// build (constant overflow) rather than miscompiling silently.
const _ = uint64(unsafe.Sizeof(uintptr(0))) - 8
