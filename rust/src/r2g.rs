//! Real rust2go integration (opt-in: `--features rust2go`).
//!
//! rust2go's `g2r` (Go-calls-Rust) generates a typed C-ABI crossing plus a Go
//! binding that calls it over rust2go's own g0-stack `asmcall`/`cgocall` shim.
//! g2r is **synchronous** — its docs say, for Go-calls-*async*-Rust, to "manually
//! spawn by your own". That is exactly what Sable provides: this handler decodes
//! the typed request via rust2go's marshalling, then **spawns onto Sable's fused
//! runtime** and delivers the result through Sable's completion doorbell on
//! `token`. So rust2go owns the typed request crossing; Sable owns the async
//! execution + completion delivery. See examples/rust2go.md.
//!
//! rust2go-cli reads THIS file to generate the Go binding
//! (`examples/rust2go-real/r2g_gen.go`).

use rust2go::R2G;
use std::time::Duration;

/// The typed request, marshalled across the FFI by rust2go (`#[derive(R2G)]`
/// generates the ref type + to/from-ref converters used on both sides). A stock
/// lookup: order `qty` units of `item`.
#[derive(R2G, Clone)]
pub struct Order {
    pub item: String,
    pub qty: u8,
}

#[rust2go::g2r]
pub trait DemoCall {
    /// Kick off an async stock check. `rt` is the Sable runtime pointer (as u64)
    /// and `token` is the Sable await token; the async result is delivered by
    /// Sable's dispatcher, not returned here (g2r is sync). Marked `#[cgo]` so the
    /// crossing uses the SAFE cgo shim (the spawn allocates a task).
    #[cgo]
    fn demo_check_spawn(order: Order, rt: u64, token: u64);
}

impl DemoCall for DemoCallImpl {
    fn demo_check_spawn(order: Order, rt: u64, token: u64) {
        // Reconstitute the Sable runtime handle passed from Go.
        let rt = unsafe { &*(rt as *const crate::SableRuntime) };
        let inner = rt.inner.clone();
        inner.note_spawn(); // account the admission (in-flight gauge / stats)

        rt.handle.spawn(async move {
            // The genuinely-async work a goroutine awaits without pinning an OS
            // thread — running on the runtime FUSED into Go's netpoll (a timer
            // stands in for an inventory/DB round-trip).
            tokio::time::sleep(Duration::from_millis(1)).await;

            const STOCK: u8 = 20; // units on hand
            let ok = order.qty <= STOCK;
            let message = if ok {
                format!("{} reserved", order.item)
            } else {
                format!("only {} {} in stock", STOCK, order.item)
            };

            // Deliver the response via Sable's async completion path (the result
            // is a Rust-owned buffer Go copies out, then frees — same contract as
            // the generic Call). Encoding mirrors the Go example's decodeReply.
            let bytes = encode_reply(ok, &message);
            let handle = Box::into_raw(Box::new(crate::CallResult { ok: true, bytes })) as u64;
            inner.complete(token, handle);
        });
    }
}

/// [ok:u8][msg_len:u8][message:utf8] — the same flat codec the Go example uses.
fn encode_reply(ok: bool, message: &str) -> Vec<u8> {
    let msg = message.as_bytes();
    let msg = &msg[..msg.len().min(255)];
    let mut out = Vec::with_capacity(2 + msg.len());
    out.push(u8::from(ok));
    out.push(msg.len() as u8);
    out.extend_from_slice(msg);
    out
}
