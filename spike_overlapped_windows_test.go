//go:build windows

package sable

// spike_overlapped_windows_test.go — the Go-only de-risking spike for Windows
// IOCP fd-fusion (no Rust FFI). It proves the load-bearing assumption before the
// full feature is built: that an OVERLAPPED named-pipe read handle wrapped by
// os.NewFile routes its reads through the runtime's single IOCP (parking in the
// netpoller) rather than blocking one OS thread per read. Runs in BOTH windows CI
// jobs (portable and fast) because it depends on no build-tagged Sable symbols.
//
// This can only be validated on a native windows/amd64 runner — the arm64 dev box
// cross-compiles it but cannot run it (Docker amd64 is qemu-user, which segfaults
// on complex Go binaries).

import (
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// dribble writes nbytes to w in `chunks` writes with a gap between them (forcing
// the reader to make progress across several completions), then closes w.
func dribble(w syscall.Handle, nbytes, chunks int) {
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
			time.Sleep(200 * time.Microsecond)
		}
	}
}

// readAll loops Read until it has `n` bytes or hits EOF/error, returning the count.
func readAll(rf *os.File, n int) int {
	buf := make([]byte, n)
	got := 0
	for got < n {
		m, err := rf.Read(buf[got:])
		got += m
		if err != nil {
			break
		}
	}
	return got
}

// TestSpikeOverlappedPipeRoutesThroughIOCP: the basic path — an overlapped pipe,
// wrapped by os.NewFile, read to completion while a writer dribbles bytes.
func TestSpikeOverlappedPipeRoutesThroughIOCP(t *testing.T) {
	r, w, err := overlappedPipe()
	if err != nil {
		t.Fatalf("overlappedPipe: %v", err)
	}
	rf := os.NewFile(uintptr(r), "spike-read")
	if rf == nil {
		t.Fatal("os.NewFile returned nil")
	}
	defer rf.Close()

	const n = 4096
	go dribble(w, n, 4)
	if got := readAll(rf, n); got != n {
		t.Fatalf("got %d bytes, want %d", got, n)
	}
}

// TestSpikePreBufferedThenClosed: everything is written AND the write end closed
// before the first Read — the buffered bytes must still be readable (then EOF).
// This is the Windows analog of the write-before-registration race.
func TestSpikePreBufferedThenClosed(t *testing.T) {
	r, w, err := overlappedPipe()
	if err != nil {
		t.Fatalf("overlappedPipe: %v", err)
	}
	rf := os.NewFile(uintptr(r), "spike-read")
	if rf == nil {
		t.Fatal("os.NewFile returned nil")
	}
	defer rf.Close()

	const n = 4096
	var done uint32
	if err := syscall.WriteFile(w, make([]byte, n), &done, nil); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	syscall.CloseHandle(w)
	time.Sleep(20 * time.Millisecond) // let the close be observed

	if got := readAll(rf, n); got != n {
		t.Fatalf("pre-buffered got %d, want %d", got, n)
	}
}

// TestSpikeBoundedThreads: the invariant — many concurrent fused reads must NOT
// each pin an OS thread. Each read is held in-flight (writer sends one byte, then
// sleeps before the rest) so all `conc` reads are simultaneously parked when we
// sample the thread count. Under IOCP routing the delta is O(GOMAXPROCS); under a
// blocking thread-per-read fallback it would be ~conc.
func TestSpikeBoundedThreads(t *testing.T) {
	const conc = 256

	base, err := processThreadCount()
	if err != nil {
		t.Skipf("processThreadCount unavailable: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, w, err := overlappedPipe()
			if err != nil {
				t.Errorf("overlappedPipe: %v", err)
				return
			}
			rf := os.NewFile(uintptr(r), "spike-read")
			if rf == nil {
				syscall.CloseHandle(r)
				syscall.CloseHandle(w)
				t.Errorf("os.NewFile returned nil")
				return
			}
			defer rf.Close()
			go holdWriter(w)
			if got := readAll(rf, 4096); got != 4096 {
				t.Errorf("got %d want 4096", got)
			}
		}()
	}

	// Sample while all reads are parked mid-stream (writers are in their sleep).
	time.Sleep(150 * time.Millisecond)
	peak, err := processThreadCount()
	wg.Wait()
	if err != nil {
		t.Skipf("processThreadCount (peak) unavailable: %v", err)
	}

	delta := peak - base
	t.Logf("thread count: base=%d peak=%d delta=%d (conc=%d)", base, peak, delta, conc)
	// Catastrophic thread-per-read shows delta ≈ conc; IOCP shows a handful. Fail
	// only on the unambiguous thread-per-read signal for now; tighten toward
	// conc/2 once real runner numbers are observed.
	if delta > conc*3/4 {
		t.Errorf("thread count grew by %d under %d concurrent reads — looks like "+
			"thread-per-read (blocking ReadFile), not IOCP routing", delta, conc)
	}
}

// holdWriter writes one byte, sleeps to hold the reader's overlapped read
// in-flight, then writes the remaining 4095 and closes.
func holdWriter(w syscall.Handle) {
	defer syscall.CloseHandle(w)
	var done uint32
	_ = syscall.WriteFile(w, []byte{1}, &done, nil)
	time.Sleep(400 * time.Millisecond)
	_ = syscall.WriteFile(w, make([]byte, 4095), &done, nil)
}
