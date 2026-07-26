//go:build windows

package sable

// overlapped_pipe_windows.go — a Windows anonymous-pipe equivalent whose READ end
// is opened FILE_FLAG_OVERLAPPED, so os.NewFile registers it with the runtime's
// single IOCP (Go's netpoller) rather than treating it as a blocking synchronous
// handle. This is the foreign fd whose I/O completion Sable fuses onto Go's one
// event loop on Windows (the completion-model counterpart of the Unix readiness
// pump; see poll.go / reactor.rs).
//
// syscall.CreatePipe (what os.Pipe uses) produces synchronous handles, so we use
// the standard named-pipe workaround: a uniquely-named single-instance server
// pipe created OVERLAPPED, plus a client handle that connects to it. Raw kernel32
// via syscall.NewLazyDLL keeps the repo free of a golang.org/x/sys dependency
// (bridge_dup_windows.go already sticks to package syscall).

import (
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// modkernel32 is shared by the Windows raw-syscall helpers in this package
// (overlappedPipe here, processThreadCount in thread_count_windows.go).
var modkernel32 = syscall.NewLazyDLL("kernel32.dll")

var (
	procCreateNamedPipeW = modkernel32.NewProc("CreateNamedPipeW")
	procCreateFileW      = modkernel32.NewProc("CreateFileW")
	procConnectNamedPipe = modkernel32.NewProc("ConnectNamedPipe")
)

const (
	_PIPE_ACCESS_INBOUND   = 0x00000001
	_FILE_FLAG_OVERLAPPED  = 0x40000000
	_PIPE_TYPE_BYTE        = 0x00000000
	_PIPE_WAIT             = 0x00000000
	_GENERIC_WRITE         = 0x40000000
	_OPEN_EXISTING         = 3
	_FILE_ATTRIBUTE_NORMAL = 0x00000080
	_ERROR_PIPE_CONNECTED  = 535
	_pipeBufSize           = 64 * 1024
)

// pipeCtr makes each pipe name unique within the process.
var pipeCtr atomic.Uint64

// overlappedPipe returns a connected byte-pipe pair: r is an OVERLAPPED server
// (read) handle suitable for os.NewFile → IOCP registration; w is a plain
// synchronous client (write) handle. The caller owns both handles.
func overlappedPipe() (r, w syscall.Handle, err error) {
	name := fmt.Sprintf(`\\.\pipe\sable-%d-%d`, os.Getpid(), pipeCtr.Add(1))
	namep, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return syscall.InvalidHandle, syscall.InvalidHandle, err
	}

	rh, _, e := procCreateNamedPipeW.Call(
		uintptr(unsafe.Pointer(namep)),
		uintptr(_PIPE_ACCESS_INBOUND|_FILE_FLAG_OVERLAPPED),
		uintptr(_PIPE_TYPE_BYTE|_PIPE_WAIT),
		1,            // nMaxInstances
		_pipeBufSize, // nOutBufferSize
		_pipeBufSize, // nInBufferSize
		0,            // nDefaultTimeOut
		0,            // lpSecurityAttributes
	)
	r = syscall.Handle(rh)
	if r == syscall.InvalidHandle {
		return syscall.InvalidHandle, syscall.InvalidHandle, fmt.Errorf("CreateNamedPipeW: %w", e)
	}

	wh, _, e := procCreateFileW.Call(
		uintptr(unsafe.Pointer(namep)),
		uintptr(_GENERIC_WRITE),
		0, // no sharing
		0, // lpSecurityAttributes
		uintptr(_OPEN_EXISTING),
		uintptr(_FILE_ATTRIBUTE_NORMAL),
		0, // hTemplateFile
	)
	w = syscall.Handle(wh)
	if w == syscall.InvalidHandle {
		syscall.CloseHandle(r)
		return syscall.InvalidHandle, syscall.InvalidHandle, fmt.Errorf("CreateFileW(client): %w", e)
	}

	// The client (w) has already connected via CreateFileW, so ConnectNamedPipe
	// completes the server side synchronously with ERROR_PIPE_CONNECTED — the
	// documented "already connected" result. Calling it AFTER the client connects
	// sidesteps the NULL-lpOverlapped caveat (which only bites when no client is
	// waiting). A nonzero return also counts as connected.
	ret, _, ce := procConnectNamedPipe.Call(uintptr(r), 0)
	if ret == 0 {
		if errno, ok := ce.(syscall.Errno); !ok || errno != _ERROR_PIPE_CONNECTED {
			syscall.CloseHandle(r)
			syscall.CloseHandle(w)
			return syscall.InvalidHandle, syscall.InvalidHandle, fmt.Errorf("ConnectNamedPipe: %w", ce)
		}
	}
	return r, w, nil
}
