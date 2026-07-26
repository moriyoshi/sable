//go:build !sable_portable && unix

package sable

// api_fusion_unix.go — the fd-fusion public API, split out of api_fast.go. Both
// entry points rest on Go's netpoller sourcing readiness for a foreign fd, which
// only the Unix netpoll (epoll/kqueue) provides; the Windows fast build omits fd
// fusion (Windows netpoll is IOCP, completion-based).

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
