//go:build !sable_portable && unix

package sable

import "syscall"

// nonblockingPipe creates a pipe with both ends set nonblocking and
// close-on-exec. It replaces syscall.Pipe2, which exists on Linux but not on
// darwin/BSD; syscall.Pipe is portable, and the flags are applied via fcntl
// (SetNonblock/CloseOnExec) under the syscall.ForkLock to avoid leaking the fds
// across a concurrent fork+exec.
func nonblockingPipe(fds *[2]int) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := syscall.Pipe(fds[:]); err != nil {
		return err
	}
	for _, fd := range fds {
		syscall.CloseOnExec(fd)
		if err := syscall.SetNonblock(fd, true); err != nil {
			syscall.Close(fds[0])
			syscall.Close(fds[1])
			return err
		}
	}
	return nil
}
