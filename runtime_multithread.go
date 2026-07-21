//go:build sable_multithread

package sable

// runtime_multithread.go — S-4.2: build the fused runtime on an IO-disabled
// multi-thread executor for CPU-heavy handlers, preserving the single-epoll
// invariant. Selected with -tags sable_multithread; requires the Rust lib built
// with --features multithread (so sable_runtime_new_multithread is linkable).
//
// SABLE_EXECUTOR_THREADS sets the worker count (unset / 0 => num_cpus).

// #include "sable.h"
import "C"

import (
	"os"
	"strconv"
)

func newRuntime() *C.SableRuntime {
	n := 0
	if v := os.Getenv("SABLE_EXECUTOR_THREADS"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			n = p
		}
	}
	return C.sable_runtime_new_multithread(C.int(n))
}
