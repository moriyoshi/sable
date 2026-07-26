//go:build !sable_portable

package sable

// canary_test.go — behavioral certification of the runtime-internal ABI sable
// depends on. Each test exercises ONE linknamed dependency directly, so a
// failure points at the exact primitive a Go release broke. `make abi-check`
// runs these; if they pass on a new toolchain, it is safe to add to
// SupportedGoVersions (guard.go). These are the teeth behind the commitment to
// track Go's internals — the machine, not a human re-audit, catches drift.
//
// These two canaries are OS-neutral (the atomics and gopark/goready are Go
// runtime symbols, not fd machinery), so they also certify the ABI on the
// Windows fast build. The netpoll canary is Unix-only (canary_fusion_unix_test.go).

import (
	"testing"
	"time"
	"unsafe"
)

// internal/runtime/atomic.{Cas,Xchg,Load,Store64,Load64}
func TestCanaryInternalAtomics(t *testing.T) {
	var u uint32 = 5
	if !atomicCas(&u, 5, 9) {
		t.Fatal("atomicCas(5->9) failed")
	}
	if got := atomicLoad(&u); got != 9 {
		t.Fatalf("atomicLoad = %d, want 9", got)
	}
	if old := atomicXchg(&u, 3); old != 9 {
		t.Fatalf("atomicXchg old = %d, want 9", old)
	}
	if got := atomicLoad(&u); got != 3 {
		t.Fatalf("atomicLoad after Xchg = %d, want 3", got)
	}
	var v uint64
	atomicStore64(&v, 0xdead_beef_cafe)
	if got := atomicLoad64(&v); got != 0xdead_beef_cafe {
		t.Fatalf("atomicLoad64 = %#x, want 0xdeadbeefcafe", got)
	}
}

// runtime.gopark / runtime.goready — round-trip via the await-slot machinery
// (parkCommit on g0 + goready inside deliverCompletion).
func TestCanaryGoparkGoready(t *testing.T) {
	slot := &awaitSlot{}
	token := tokenCtr.Add(1)
	awaits.Store(token, slot)

	go func() {
		time.Sleep(20 * time.Millisecond)
		deliverCompletion(token, 4242) // publishes result + goready(slot.gp)
	}()

	gopark(parkCommit, unsafe.Pointer(slot), waitReasonSable, traceBlockSable, 1)
	_ = atomicLoad(&slot.state) // acquire
	if got := atomicLoad64(&slot.result); got != 4242 {
		t.Fatalf("gopark/goready round-trip = %d, want 4242", got)
	}
}
