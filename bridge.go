package sable

// bridge.go — the CORE Go<->Rust plumbing (present in every build, including the
// portable zero-linkname one): the cgo preamble, runtime lifecycle, the
// completion dispatcher, and the core "Go awaits Rust" wrapper. The
// reactor/pump-dependent parts live in bridge_fast.go (build tag !sable_portable).

// #cgo CFLAGS: -I${SRCDIR}/include
// #include "sable.h"
import "C"

// NOTE: the cgo LDFLAGS (which staticlib to link) live in link_default.go /
// link_extern.go so an embedder can own the combined staticlib (S-5). Default
// build links sable's own libsable.a; -tags sable_extern_lib omits it so the
// embedder supplies a combined lib (imbh + sable runtime + handlers) via
// CGO_LDFLAGS.

import (
	"errors"
	"io"
	"os"
	"sync/atomic"
	"syscall"
)

var (
	rt             *C.SableRuntime
	completionFile *os.File
	dispatcherDone chan struct{} // closed when the dispatcher goroutine exits
	// tokenCtr is shared by both await backends (park.go / park_portable.go) and
	// by bridge_fast.go; `awaits` (whose value type differs per backend) lives in
	// each backend file.
	tokenCtr atomic.Uint64
)

// sableAdd is the M0 smoke test: a round-trip across the FFI boundary.
func sableAdd(a, b int64) int64 {
	return int64(C.sable_add(C.int64_t(a), C.int64_t(b)))
}

// newAndFreeRuntime creates a fresh SableRuntime and immediately frees it,
// exercising the teardown path. Does not touch the global rt.
func newAndFreeRuntime() {
	r := C.sable_runtime_new()
	if r == nil {
		panic("sable_runtime_new failed")
	}
	C.sable_runtime_free(r)
}

// sableInit builds the runtime and starts the dispatcher goroutine. Safe to
// call once (from main or TestMain).
func sableInit() {
	requireVerifiedGo() // fail closed on an uncertified internal ABI
	rt = C.sable_runtime_new()
	if rt == nil {
		panic("sable_runtime_new failed")
	}
	efd := int(C.sable_completion_efd(rt))
	// DUP the doorbell eventfd: Rust owns and closes the original on teardown,
	// Go owns and closes this dup. Both refer to the same eventfd object (Rust's
	// writes are visible to Go's reads), but each side closes its own descriptor,
	// so shutdown never double-closes. os.NewFile routes reads through the
	// netpoller (SUPPORTED os API — no linkname); Read parks the goroutine.
	dupfd, err := syscall.Dup(efd)
	if err != nil {
		panic("dup(completion eventfd): " + err.Error())
	}
	f := os.NewFile(uintptr(dupfd), "sable-completion")
	if f == nil {
		panic("os.NewFile(completion eventfd) failed")
	}
	completionFile = f
	dispatcherDone = make(chan struct{})
	go dispatcher(f)
}

// dispatcher parks on the completion doorbell in the netpoller; each wakeup it
// drains ALL queued completions (drain-to-empty keeps the doorbell
// lost-wakeup-free even when writes coalesce on the eventfd counter).
func dispatcher(f *os.File) {
	defer close(dispatcherDone)
	buf := make([]byte, 8)
	for {
		if _, err := f.Read(buf); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				// Doorbell closed (Shutdown): drain any final queued completions
				// (e.g. cancellations from the executor teardown) and exit.
				drainCompletions()
				return
			}
			continue
		}
		drainCompletions()
	}
}

func drainCompletions() {
	for {
		var token, result C.uint64_t
		if C.sable_next_completion(rt, &token, &result) == 0 {
			return
		}
		deliverCompletion(uint64(token), uint64(result))
	}
}

// awaitRust: a goroutine awaits a Rust async result, delivered by the dispatcher.
// The park/deliver mechanism is provided by the selected backend (gopark/goready
// in park.go, or channels in park_portable.go).
func awaitRust(kind uint32, arg uint64) uint64 {
	return awaitViaPark(func(token uint64) {
		C.sable_spawn_await(rt, C.uint32_t(kind), C.uint64_t(arg), C.uint64_t(token))
	})
}
