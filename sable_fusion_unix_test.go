//go:build !sable_portable && unix

package sable

import "testing"

// sable_fusion_unix_test.go — the fd-fusion acceptance tests, split out of
// sable_test.go. They exercise the reactor/pump (readiness from Go's netpoll),
// the eventfd value channel, and the single-shared-epoll invariant — all Unix-
// only. The Windows fast build omits fd fusion, so these do not compile there.

// TestRustAwaitsGo — direction (ii), single shot (eventfd value channel).
func TestRustAwaitsGo(t *testing.T) {
	skipNonLinux(t, "rust_awaits_go (eventfd value channel)")
	if got := demoRustAwaitsGo(0, 0); got != 7 {
		t.Fatalf("demoRustAwaitsGo(0,0) = %d, want 7", got)
	}
}

// TestSingleEpoll is the defining M2 assertion: after exercising both the
// completion doorbell and a fused fd, there is exactly ONE epoll in the process
// (Go's netpoll). tokio, running IO-disabled, owns none.
func TestSingleEpoll(t *testing.T) {
	skipNonLinux(t, "the single-shared-epoll invariant")
	if got := awaitRust(0, 0); got != 42 { // ensures the poller is initialized
		t.Fatalf("awaitRust = %d, want 42", got)
	}
	if got := demoRustAwaitsGo(0, 0); got != 7 { // exercises GoAsyncFd on the epoll
		t.Fatalf("demoRustAwaitsGo = %d, want 7", got)
	}
	if n := countEpollFds(); n != 1 {
		t.Fatalf("expected exactly 1 epoll fd (Go's netpoll), found %d", n)
	}
}

// TestGoNetpollDrivesRustRead drives a real tokio read whose readiness comes
// only from Go's netpoll, across multiple chunks (exercises the edge-triggered
// re-arm/drain loop).
func TestGoNetpollDrivesRustRead(t *testing.T) {
	const n = 4096
	if got := readPipeViaRust(n, 4); got != uint64(n) {
		t.Fatalf("readPipeViaRust = %d bytes, want %d", got, n)
	}
}

// TestStressRustAwaitsGo fans out many concurrent Go->Rust->Go awaits (both
// directions per iteration). In M2 each iteration registers a fresh fd with the
// netpoller and drives pump control via cgo callbacks serialized on the single
// executor thread, so this is deliberately more modest than the doorbell path.
func TestStressRustAwaitsGo(t *testing.T) {
	n := 2000
	if testing.Short() {
		n = 200
	}
	stress(t, n, awaitRustAwaitsGo)
}
