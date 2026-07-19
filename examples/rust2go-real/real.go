//go:build sable_rust2go

// NOTE: rust2go deliberately passes Go pointers through its ref structs, so this
// program MUST be RUN with GODEBUG=cgocheck=0,invalidptr=0 to relax Go's cgo
// pointer checks (rust2go's own README/CI require the same). GODEBUG is read at
// process start, so it can't be baked in; the Makefile run-rust2go/test-rust2go
// targets set it. This is scoped to this opt-in example only — the sable library
// and its default/portable builds keep every cgo check fully on.

package main

// real.go — the REAL rust2go integration (opt-in: -tags sable_rust2go, linked
// against the Rust lib built with --features rust2go). Pairs with the generated
// binding r2g_gen.go and the Rust trait in rust/src/r2g.rs.
//
// Flow:  Go --(rust2go g2r typed cgo crossing)--> Rust handler --(spawn)-->
//        sable fused runtime --(async work + completion doorbell)--> dispatcher
//        --(goready)--> the parked goroutine.
//
// rust2go owns the TYPED REQUEST crossing (its strength, and sync-only); sable
// owns the ASYNC execution + completion delivery (which rust2go's g2r docs
// explicitly delegate: "manually spawn by your own"). See examples/rust2go.md.

import (
	"errors"
	"runtime"

	"github.com/moriyoshi/sable"
)

// StockReply is the typed reply (a local copy of the illustrative example's type
// — examples stay self-contained). Order is the request type, defined by the
// generated binding r2g_gen.go (from rust/src/r2g.rs).
type StockReply struct {
	OK      bool
	Message string
}

// decodeReply parses [ok:u8][msg_len:u8][message:utf8].
func decodeReply(b []byte) (StockReply, error) {
	if len(b) < 2 {
		return StockReply{}, errors.New("malformed StockReply: short")
	}
	msgLen := int(b[1])
	if len(b) < 2+msgLen {
		return StockReply{}, errors.New("malformed StockReply: truncated message")
	}
	return StockReply{OK: b[0] != 0, Message: string(b[2 : 2+msgLen])}, nil
}

// CheckStockReal issues a typed, async stock check across the fusion boundary via
// the real rust2go g2r binding, returning the typed StockReply. The goroutine
// parks (inside sable.AwaitCallResult) until sable delivers the result; sable
// then copies the CallResult buffer out and frees it.
func CheckStockReal(item string, qty uint8) (StockReply, error) {
	rtU64 := uint64(sable.RuntimePtr()) // rust2go g2r takes the runtime as a u64
	order := Order{item: item, qty: qty}

	// The spawn closure runs synchronously, kicking off the Rust work via
	// rust2go's generated binding; sable parks the goroutine on the token and
	// returns the delivered result bytes.
	out, err := sable.AwaitCallResult(func(token uint64) {
		tok := token
		DemoCallImpl{}.demo_check_spawn(&order, &rtU64, &tok)
	})
	runtime.KeepAlive(order)
	if err != nil {
		return StockReply{}, err
	}
	return decodeReply(out)
}

// runDemo runs the real-rust2go path (from main, only under the tag).
func runDemo() {
	for _, o := range []struct {
		item string
		qty  uint8
	}{{"widgets", 5}, {"gizmos", 200}} {
		reply, err := CheckStockReal(o.item, o.qty)
		if err != nil {
			println("  [rust2go g2r -> sable] CheckStockReal error:", err.Error())
			continue
		}
		ok := "false"
		if reply.OK {
			ok = "true"
		}
		println("  [rust2go g2r -> sable] CheckStockReal(" + o.item + ") -> ok=" + ok + " message=" + reply.Message)
	}
}
