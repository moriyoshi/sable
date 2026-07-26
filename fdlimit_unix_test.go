//go:build !sable_portable && unix

package sable

import "syscall"

// raiseFDLimit lifts the soft NOFILE limit to the hard cap so the Rust-awaits-Go
// stress (which creates many transient eventfds/pipe fds) does not exhaust the
// descriptor table. Unix-only; the Windows fast build creates no such fds.
func raiseFDLimit() {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err == nil {
		lim.Cur = lim.Max
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim)
	}
}
