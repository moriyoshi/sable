//go:build !sable_portable

package sable

// canary_test.go — behavioral certification of the runtime-internal ABI sable
// depends on. Each test exercises ONE linknamed dependency directly, so a
// failure points at the exact primitive a Go release broke. `make abi-check`
// runs these; if they pass on a new toolchain, it is safe to add to
// SupportedGoVersions (guard.go). These are the teeth behind the commitment to
// track Go's internals — the machine, not a human re-audit, catches drift.

import (
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// internal/runtime/atomic.{Cas,Xchg,Load,Store64,Load64}
func TestCanaryInternalAtomics(t *testing.T) {
	var u uint32 = 5
	if !atomicCas(&u, 5, 9) {
		t.Fatal("atomicCas(5->9) failed")
	}
	if got := atomicLoad(&u); got != 9 {
		t.Fatalf("atomicLoad = %d, want 9", got)
	}
	if old := atomicXchg(&u, 3); old != 9 {
		t.Fatalf("atomicXchg old = %d, want 9", old)
	}
	if got := atomicLoad(&u); got != 3 {
		t.Fatalf("atomicLoad after Xchg = %d, want 3", got)
	}
	var v uint64
	atomicStore64(&v, 0xdead_beef_cafe)
	if got := atomicLoad64(&v); got != 0xdead_beef_cafe {
		t.Fatalf("atomicLoad64 = %#x, want 0xdeadbeefcafe", got)
	}
}

// runtime.gopark / runtime.goready — round-trip via the await-slot machinery
// (parkCommit on g0 + goready inside deliverCompletion).
func TestCanaryGoparkGoready(t *testing.T) {
	slot := &awaitSlot{}
	token := tokenCtr.Add(1)
	awaits.Store(token, slot)

	go func() {
		time.Sleep(20 * time.Millisecond)
		deliverCompletion(token, 4242) // publishes result + goready(slot.gp)
	}()

	gopark(parkCommit, unsafe.Pointer(slot), waitReasonSable, traceBlockSable, 1)
	_ = atomicLoad(&slot.state) // acquire
	if got := atomicLoad64(&slot.result); got != 4242 {
		t.Fatalf("gopark/goready round-trip = %d, want 4242", got)
	}
}

// internal/poll.runtime_poll{Open,Wait,Unblock,Close} — register a pipe read-end
// with the netpoller, park in pollWait, wake it by writing the pipe.
func TestCanaryNetpoll(t *testing.T) {
	var fds [2]int
	if err := syscall.Pipe2(fds[:], syscall.O_NONBLOCK|syscall.O_CLOEXEC); err != nil {
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
