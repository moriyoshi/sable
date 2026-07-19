//go:build !sable_extern_lib

package sable

// demohandle.go — a thin Go wrapper over the demo handle op's release, used by
// the S-2 handle test. Tagged !sable_extern_lib so it is absent from an
// embedder-owned build (which may compile the Rust core WITHOUT the `demo`
// feature, so sable_demo_handle_free would not exist to link against).

// #include "sable.h"
import "C"

// demoHandleFree drives the release of a handle produced by the demo handle op
// (the caller's stand-in for driving a real Arrow stream's own release).
func demoHandleFree(ptr uintptr) { C.sable_demo_handle_free(C.uint64_t(ptr)) }
