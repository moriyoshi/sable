//go:build !sable_portable && windows

package sable

// bridge_fusion_windows.go — the Windows fd-fusion plumbing, the completion-model
// counterpart of bridge_fusion_unix.go. Windows netpoll is IOCP (completion-based,
// not readiness-based), so the contract is inverted: **Go owns the overlapped read
// through the runtime's single IOCP** (os.File.Read parks in the netpoller and
// wakes on completion) and delivers the byte count to a Rust tokio task, which
// awaits it via win_reactor. No fd is ever handed to Rust — Go owns both pipe ends
// for their whole life, so the Unix fd-reuse race class does not exist here.

// #include "sable.h"
import "C"

import (
	"os"
	"sync/atomic"
	"syscall"
	"time"
)

// winReactorCtr gives each fused read a unique regid (a plain u64 identity, never
// a handle — so no 32-bit-narrowing concern).
var winReactorCtr atomic.Uint64

// readPipeViaRust has a Rust tokio task await a pipe read whose bytes are read by
// Go through the single runtime IOCP, returning the byte count. It mirrors the
// Unix ReadPipeViaRust contract (same signature, same "bytes read" result), but
// Go performs the read (IOCP completion) instead of Rust (readiness edge).
func readPipeViaRust(nbytes, chunks int) uint64 {
	return readPipeViaRustSplit(nbytes, nbytes, chunks)
}

// readPipeViaRustSplit has the Rust task await `want` bytes while the writer sends
// `have` (want==have is the normal case; have<want exercises the EOF/partial path).
func readPipeViaRustSplit(want, have, chunks int) uint64 {
	r, w, err := overlappedPipe()
	if err != nil {
		panic(err)
	}
	go writeChunks(w, have, chunks)

	regid := winReactorCtr.Add(1)
	rf := os.NewFile(uintptr(r), "sable-ovl-read")
	if rf == nil {
		syscall.CloseHandle(r)
		panic("os.NewFile(overlapped pipe) failed")
	}
	go readPump(rf, want, regid)

	return awaitViaPark(func(token uint64) {
		C.sable_spawn_read_handle(rt, C.uint64_t(regid), C.uint64_t(token))
	})
}

// readPump reads up to `nbytes` from rf via the runtime IOCP (os.File.Read parks
// in the netpoller), then delivers the count to Rust. rf owns the read handle;
// rf.Close deregisters it from the IOCP and closes it. A completion whose tokio
// task was already dropped is a harmless no-op on the Rust side (win_reactor finds
// no registration).
func readPump(rf *os.File, nbytes int, regid uint64) {
	defer rf.Close()
	buf := make([]byte, nbytes)
	got := 0
	for got < nbytes {
		n, err := rf.Read(buf[got:])
		if n > 0 {
			got += n
		}
		if err != nil {
			break // io.EOF (writer closed) or a real error → deliver the partial count
		}
	}
	C.sable_fd_read_complete(C.uint64_t(regid), C.uint64_t(got))
}

// writeChunks dribbles `nbytes` into w in `chunks` writes (forcing several IOCP
// completions on the read side), then closes w (which the reader observes as EOF).
func writeChunks(w syscall.Handle, nbytes, chunks int) {
	defer syscall.CloseHandle(w)
	buf := make([]byte, nbytes)
	per := nbytes / chunks
	if per <= 0 {
		per = nbytes
	}
	var done uint32
	for off := 0; off < nbytes; {
		end := off + per
		if end > nbytes {
			end = nbytes
		}
		_ = syscall.WriteFile(w, buf[off:end], &done, nil)
		off = end
		if off < nbytes {
			time.Sleep(200 * time.Microsecond) // force multiple readiness/completion edges
		}
	}
}
