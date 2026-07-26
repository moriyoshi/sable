//go:build !sable_portable && !unix

package sable

// raiseFDLimit is a no-op off Unix. The Windows fast build has no fd-fusion path
// (no transient eventfds/pipe fds), and Windows has no RLIMIT_NOFILE knob; the
// default handle limit is far above what the OS-neutral suite needs.
func raiseFDLimit() {}
