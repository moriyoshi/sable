//go:build !sable_portable && unix

package sable

// canary_fusion_unix_test.go — the netpoll ABI canary, split out of
// canary_test.go. It certifies the internal/poll.runtime_poll* linkname surface
// the fd-fusion pump depends on, which is Unix-only (Windows netpoll is IOCP, so
// the Windows fast build has no pump and does not link these symbols).

import (
	"syscall"
	"testing"
	"time"
)

// internal/poll.runtime_poll{Open,Wait,Unblock,Close} — register a pipe read-end
// with the netpoller, park in pollWait, wake it by writing the pipe.
func TestCanaryNetpoll(t *testing.T) {
	var fds [2]int
	if err := nonblockingPipe(&fds); err != nil {
		t.Fatal(err)
	}
	r, w := fds[0], fds[1]
	defer syscall.Close(w)

	pd, errno := poll_runtime_pollOpen(uintptr(r))
	if errno != 0 {
		t.Fatalf("poll_runtime_pollOpen errno=%d", errno)
	}

	rc := make(chan int, 1)
	go func() { rc <- poll_runtime_pollWait(pd, pollModeRead) }()

	time.Sleep(20 * time.Millisecond)
	if _, err := syscall.Write(w, []byte{1}); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-rc:
		if got != pollNoError {
			t.Fatalf("poll_runtime_pollWait rc=%d, want %d", got, pollNoError)
		}
	case <-time.After(2 * time.Second):
		poll_runtime_pollUnblock(pd)
		t.Fatal("poll_runtime_pollWait never woke — netpoll linkname broken")
	}

	poll_runtime_pollUnblock(pd) // required before pollClose
	poll_runtime_pollClose(pd)
	var b [8]byte
	_, _ = syscall.Read(r, b[:])
	syscall.Close(r)
}
