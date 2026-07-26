//go:build windows

package sable

// thread_count_windows.go — a raw-kernel32 process thread counter, the Windows
// stand-in for the Unix "single shared epoll" evidence. On Linux we assert exactly
// one [eventpoll] in /proc/self/fd; Windows has no such view. Instead we assert
// that N concurrent fused reads do NOT spawn ~N blocked OS threads: if os.NewFile
// routes the overlapped read through the runtime's single IOCP (the intended
// path), the reads park in the netpoller and the thread count stays ~GOMAXPROCS;
// if it instead fell back to a synchronous blocking ReadFile, each in-flight read
// would pin its own OS thread (thread-per-read), which is exactly what we forbid.
//
// CreateToolhelp32Snapshot + Thread32First/Next is the documented way to walk the
// system thread table; we count entries owned by this process. Raw kernel32 keeps
// the repo free of a golang.org/x/sys dependency.

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	procCreateToolhelp32Snapshot = modkernel32.NewProc("CreateToolhelp32Snapshot")
	procThread32First            = modkernel32.NewProc("Thread32First")
	procThread32Next             = modkernel32.NewProc("Thread32Next")
)

const (
	_TH32CS_SNAPTHREAD    = 0x00000004
	_INVALID_HANDLE_VALUE = ^uintptr(0)
)

// threadEntry32 mirrors Win32 THREADENTRY32 (stable layout).
type threadEntry32 struct {
	Size           uint32
	CntUsage       uint32
	ThreadID       uint32
	OwnerProcessID uint32
	BasePri        int32
	DeltaPri       int32
	Flags          uint32
}

// processThreadCount returns the number of OS threads owned by this process.
func processThreadCount() (int, error) {
	snap, _, e := procCreateToolhelp32Snapshot.Call(uintptr(_TH32CS_SNAPTHREAD), 0)
	if snap == _INVALID_HANDLE_VALUE {
		return 0, fmt.Errorf("CreateToolhelp32Snapshot: %w", e)
	}
	defer syscall.CloseHandle(syscall.Handle(snap))

	pid := uint32(os.Getpid())
	var te threadEntry32
	te.Size = uint32(unsafe.Sizeof(te))

	ret, _, e := procThread32First.Call(snap, uintptr(unsafe.Pointer(&te)))
	if ret == 0 {
		return 0, fmt.Errorf("Thread32First: %w", e)
	}
	n := 0
	for {
		if te.OwnerProcessID == pid {
			n++
		}
		ret, _, _ := procThread32Next.Call(snap, uintptr(unsafe.Pointer(&te)))
		if ret == 0 {
			break // ERROR_NO_MORE_FILES ends the walk
		}
	}
	return n, nil
}
