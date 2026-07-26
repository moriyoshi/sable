//go:build windows

package sable

import "syscall"

// dupDoorbell duplicates the completion-doorbell HANDLE so Go owns a copy
// independent of the one Rust closes on teardown (see sableInit). On Windows the
// doorbell is an anonymous-pipe read HANDLE that Rust hands over 32-bit-narrowed
// as a c_int (Win32 handles are 32-bit-significant for interop); DuplicateHandle
// gives Go its own, which os.NewFile then blocking-reads.
func dupDoorbell(efd int) (uintptr, error) {
	p, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, err
	}
	var dup syscall.Handle
	if err := syscall.DuplicateHandle(p, syscall.Handle(efd), p, &dup, 0, false, syscall.DUPLICATE_SAME_ACCESS); err != nil {
		return 0, err
	}
	return uintptr(dup), nil
}
