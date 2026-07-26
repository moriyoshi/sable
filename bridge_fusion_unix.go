//go:build !sable_portable && unix

package sable

// bridge_fusion_unix.go — the fd-fusion fast-path plumbing split out of
// bridge_fast.go: a Rust tokio task reading a pipe whose readiness is sourced
// entirely from Go's netpoller, plus the eventfd value channel behind the "Rust
// awaits Go" compute demo. Unix-only — both rest on the netpoller's readiness
// model (epoll/kqueue), which Windows (IOCP, completion-based) does not provide.

// #include "sable.h"
import "C"

import (
	"encoding/binary"
	"syscall"
	"time"
)

// readPipeViaRust creates a nonblocking pipe, dribbles `nbytes` into it in
// `chunks` writes, and has a Rust tokio task read them back — readiness sourced
// entirely from Go's netpoll. Returns the number of bytes Rust read.
func readPipeViaRust(nbytes, chunks int) uint64 {
	var fds [2]int
	if err := nonblockingPipe(&fds); err != nil {
		panic(err)
	}
	rfd, wfd := fds[0], fds[1]

	go func() {
		defer syscall.Close(wfd)
		buf := make([]byte, nbytes)
		per := nbytes / chunks
		if per == 0 {
			per = nbytes
		}
		for off := 0; off < nbytes; {
			end := off + per
			if end > nbytes {
				end = nbytes
			}
			writeAll(wfd, buf[off:end])
			off = end
			if off < nbytes {
				time.Sleep(200 * time.Microsecond) // force multiple readiness edges
			}
		}
	}()

	got := awaitViaPark(func(token uint64) {
		C.sable_spawn_read_pipe(rt, C.int(rfd), C.uint64_t(nbytes), C.uint64_t(token))
	})
	// rfd is owned and closed by the pump (after pollClose); do not close here.
	return got
}

func writeAll(fd int, b []byte) {
	for len(b) > 0 {
		n, err := syscall.Write(fd, b)
		if n > 0 {
			b = b[n:]
			continue
		}
		if err == syscall.EINTR || err == syscall.EAGAIN {
			continue
		}
		return
	}
}

//export sable_go_compute
func sable_go_compute(kind C.uint32_t, arg C.uint64_t, efd C.int) {
	// Called from a tokio worker thread (foreign thread -> cgo needm). Return
	// promptly: hand the work to a goroutine on Go's scheduler.
	k, a, fd := uint32(kind), uint64(arg), int(efd)
	go func() {
		res := goCompute(k, a)
		writeEventfd(fd, res+1) // result rides the eventfd counter as res+1
	}()
}

// goCompute is the Go-side "work" a tokio task can await.
//
//	kind 0 -> sleep 1ms, return 7 ; kind 1 -> echo arg after an arg-derived delay
func goCompute(kind uint32, arg uint64) uint64 {
	switch kind {
	case 0:
		time.Sleep(time.Millisecond)
		return 7
	case 1:
		time.Sleep(time.Duration(arg%50) * time.Microsecond)
		return arg
	default:
		return 0
	}
}

func writeEventfd(fd int, v uint64) {
	var buf [8]byte
	binary.NativeEndian.PutUint64(buf[:], v)
	for {
		if _, err := syscall.Write(fd, buf[:]); err != syscall.EINTR {
			return
		}
	}
}
