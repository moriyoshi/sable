//go:build !sable_portable

package bench

import (
	"testing"

	"github.com/moriyoshi/sable"
)

// crossSink prevents the compiler from optimizing away the benchmarked calls.
var crossSink uint64

// BenchmarkCrossingCgo measures a raw Go->Rust crossing via the full cgo path.
func BenchmarkCrossingCgo(b *testing.B) {
	var s uint64
	for i := 0; i < b.N; i++ {
		s += sable.CrossingCgo(uint64(i))
	}
	crossSink = s
}

// BenchmarkAwaitRust measures the full end-to-end mutual-await round trip: Go
// spawns a Rust task, tokio runs it on the single shared epoll, the completion
// doorbell wakes the dispatcher, which goreadys the parked awaiter.
func BenchmarkAwaitRust(b *testing.B) {
	var s uint64
	for i := 0; i < b.N; i++ {
		s += sable.AwaitRust(1, uint64(i)+1)
	}
	crossSink = s
}
