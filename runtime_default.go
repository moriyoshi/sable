//go:build !sable_multithread

package sable

// runtime_default.go — the default single-thread executor. See
// runtime_multithread.go (built with -tags sable_multithread) for the
// IO-disabled multi-thread executor (S-4.2), which requires the Rust lib built
// with --features multithread.

// #include "sable.h"
import "C"

func newRuntime() *C.SableRuntime { return C.sable_runtime_new() }
