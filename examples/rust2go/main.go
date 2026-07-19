// Command rust2go demonstrates a rust2go-STYLE typed async call realized on
// sable's generic Call API — the calling pattern rust2go's codegen emits, hand-
// written over sable so it needs zero extra dependencies. For the REAL rust2go
// crate wired in, see examples/rust2go-real. Background: examples/rust2go.md.
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/moriyoshi/sable"
)

// Demo op ids implemented by the bundled Rust demo handlers (rust/src/demo.rs).
// A real integration would maintain its own op registry; rust2go would generate
// a per-method dispatch id.
const (
	opCheckStock uint32 = 10
	opSleepLong  uint32 = 5
)

// Order / StockReply mirror rust2go's flat-struct demo types: order Qty units of
// Item, get back whether it can be fulfilled.
type Order struct {
	Item string
	Qty  uint8
}

type StockReply struct {
	OK      bool
	Message string
}

// encodeOrder marshals an Order to the wire format check_stock expects:
// [qty:u8][item_len:u8][item:utf8]. (rust2go generates this from the struct; the
// contract is "the caller serializes its own types" — here a flat binary codec.)
func encodeOrder(o Order) []byte {
	item := []byte(o.Item)
	if len(item) > 255 {
		item = item[:255] // one length byte
	}
	b := make([]byte, 0, 2+len(item))
	b = append(b, o.Qty, byte(len(item)))
	b = append(b, item...)
	return b
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

// CheckStock is the typed, async, cancellable entry point — the analogue of a
// rust2go-generated method. The calling goroutine parks (gopark or channel, per
// backend) while the Rust future runs on the fused runtime; it is resumed by the
// dispatcher when the result lands. No OS thread is blocked, and cancelling ctx
// aborts the in-flight Rust task.
func CheckStock(ctx context.Context, o Order) (StockReply, error) {
	respBytes, err := sable.CallCtx(ctx, opCheckStock, encodeOrder(o))
	if err != nil {
		return StockReply{}, err
	}
	return decodeReply(respBytes)
}

func main() {
	sable.Init()
	for _, o := range []Order{{Item: "widgets", Qty: 5}, {Item: "gizmos", Qty: 200}} {
		reply, err := CheckStock(context.Background(), o)
		if err != nil {
			fmt.Printf("  rust2go-style CheckStock(%s) -> error: %v\n", o.Item, err)
			continue
		}
		fmt.Printf("  rust2go-style CheckStock(%s,%d) -> ok=%t message=%q\n",
			o.Item, o.Qty, reply.OK, reply.Message)
	}
	sable.Shutdown()
}
