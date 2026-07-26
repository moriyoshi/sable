//go:build !sable_portable && windows

package sable

// api_fusion_windows.go — the Windows fd-fusion public API, the completion-model
// counterpart of api_fusion_unix.go. (CountEpollFds has no Windows analog — it is
// a Linux /proc concept — so it is intentionally absent here; the demo gates its
// use to unix.)

// ReadPipeViaRust has a Rust tokio task await a pipe read whose bytes are read by
// Go through the single runtime IOCP, returning the bytes read. It demonstrates
// real fd I/O flowing through Go's one event loop on Windows — where, unlike Unix,
// Go performs the overlapped read (IOCP completion) and hands Rust the count.
func ReadPipeViaRust(nbytes, chunks int) uint64 {
	Init()
	return readPipeViaRust(nbytes, chunks)
}
