//go:build !sable_portable

package sable

// netpoll_linkname.go — the deep seam that makes Go's netpoll the single event
// loop. These pull the runtime's internal poller entry points (pushed by the
// runtime under the internal/poll.* names) so a foreign fd can be registered
// with, and waited on via, Go's one epoll.
//
// Stability: verified present and NOT on `blockedLinknames` in go1.26.4, so they
// link under the default -checklinkname=1. The exact toolchain is pinned in
// go.mod; any Go upgrade is an ABI re-audit. `*pollDesc` is runtime-internal,
// persistentalloc'd (never GC-moved/freed until pollClose), so representing it
// as a bare uintptr is safe here.
//
// The bodyless declarations are permitted because the package contains an
// assembly file (linkname_stubs.s).

import _ "unsafe"

// poll_runtime_pollOpen registers fd with the netpoller and returns an opaque
// pollDesc pointer (as uintptr) plus an errno (0 on success).
//
//go:linkname poll_runtime_pollOpen internal/poll.runtime_pollOpen
func poll_runtime_pollOpen(fd uintptr) (uintptr, int)

// poll_runtime_pollClose deregisters and frees the pollDesc.
//
//go:linkname poll_runtime_pollClose internal/poll.runtime_pollClose
func poll_runtime_pollClose(ctx uintptr)

// poll_runtime_pollReset prepares pd for a fresh wait in `mode` ('r'/'w').
// Returns 0 (ok) / 1 (closing) / 2 (timeout).
//
//go:linkname poll_runtime_pollReset internal/poll.runtime_pollReset
func poll_runtime_pollReset(ctx uintptr, mode int) int

// poll_runtime_pollWait parks the current goroutine in Go's netpoll until fd is
// ready for `mode`. Returns 0 (ok) / 1 (closing) / 2 (timeout).
//
//go:linkname poll_runtime_pollWait internal/poll.runtime_pollWait
func poll_runtime_pollWait(ctx uintptr, mode int) int

// poll_runtime_pollUnblock wakes any goroutine parked in pollWait on pd (used to
// tear a pump down cleanly).
//
//go:linkname poll_runtime_pollUnblock internal/poll.runtime_pollUnblock
func poll_runtime_pollUnblock(ctx uintptr)
