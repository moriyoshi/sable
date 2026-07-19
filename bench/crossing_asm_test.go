//go:build !sable_safe && !sable_portable

package bench

import (
	"testing"

	"github.com/moriyoshi/sable"
)

// BenchmarkCrossingAsm measures the same raw crossing via the asmcgocall fast
// path (g0 switch, no entersyscall/exitsyscall). Compare against
// BenchmarkCrossingCgo to see the savings.
func BenchmarkCrossingAsm(b *testing.B) {
	var s uint64
	for i := 0; i < b.N; i++ {
		s += sable.CrossingAsm(uint64(i))
	}
	crossSink = s
}
