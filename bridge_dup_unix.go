//go:build !windows

package sable

import "syscall"

// dupDoorbell duplicates the completion-doorbell descriptor so Go owns a copy
// independent of the one Rust closes on teardown (see sableInit). On Unix the
// doorbell is an fd (eventfd or self-pipe read end); a plain dup(2) suffices.
func dupDoorbell(efd int) (uintptr, error) {
	dupfd, err := syscall.Dup(efd)
	if err != nil {
		return 0, err
	}
	return uintptr(dupfd), nil
}
