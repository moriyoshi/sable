//go:build !sable_portable

package sable

import (
	"os"
	"strings"
)

// countEpollFds counts the epoll instances the process holds. The single-shared-
// epoll invariant (M2) is that this is exactly 1 — Go's netpoll — because the
// fused tokio runtime is built with IO disabled and owns none. Used by the
// single-epoll tests and exported as CountEpollFds for the demo.
func countEpollFds() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range entries {
		if link, err := os.Readlink("/proc/self/fd/" + e.Name()); err == nil {
			if strings.Contains(link, "[eventpoll]") {
				n++
			}
		}
	}
	return n
}
