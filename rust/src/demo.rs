//! demo.rs — the byte-buffer Call op handlers used by the demos, examples, and
//! test suite, split out of the runtime core (lib.rs).
//!
//! These are the application-level operations a real integration would replace
//! with its own registered handlers; the fusion runtime itself
//! (spawn/await/completion delivery) is op-agnostic. They are compiled into
//! every build so the generic Call path has ops to dispatch against — separated
//! by module, not by feature, to keep every build variant's flags unchanged.

use crate::Payload;
use std::time::Duration;

/// Op id for the S-2 demo handle op (exercises `Payload::Handle` end-to-end).
/// Distinct from the byte demo ops so both transports coexist.
pub(crate) const OP_HANDLE_DEMO: u32 = 30;

/// A demo handler returning an opaque Rust-owned handle: a boxed `u64` marker
/// stands in for what a real handler would return (e.g. a `*mut
/// FFI_ArrowArrayStream`). sable delivers the pointer verbatim; Go reads it, then
/// drives the release. `release`/`sable_demo_handle_free` free the box.
pub(crate) fn demo_handle(_req: Vec<u8>) -> Payload {
    let val: u64 = 0x00C0FFEE;
    let ptr = Box::into_raw(Box::new(val)) as u64;
    Payload::handle(ptr, demo_handle_release)
}

/// The release callback sable's net drives if Go never takes the handle.
unsafe extern "C" fn demo_handle_release(ptr: u64) {
    if ptr != 0 {
        drop(unsafe { Box::from_raw(ptr as *mut u64) });
    }
}

/// Go-callable free for a taken demo handle: the caller's analogue of driving the
/// Arrow stream's own release. Same drop as `demo_handle_release`. Also frees the
/// per-batch handles from the S-3 demo stream (same boxed-u64 shape).
#[unsafe(no_mangle)]
pub extern "C" fn sable_demo_handle_free(ptr: u64) {
    unsafe { demo_handle_release(ptr) };
}

/// Op id for the S-3 demo stream op.
pub(crate) const OP_STREAM_DEMO: u32 = 40;

/// A demo streaming handler: emits `n` batches (n = req[0], default 3), each an
/// opaque boxed-u64 handle (0xB000 + i), with a 1ms async gap to prove batches
/// are pulled lazily across awaits. Stops early if the receiver is dropped
/// (cursor closed) — releasing the batch it was about to hand over.
pub(crate) async fn demo_stream(req: Vec<u8>, tx: crate::BatchSender) {
    let n = req.first().copied().unwrap_or(3);
    for i in 0..n {
        tokio::time::sleep(Duration::from_millis(1)).await; // async batch production
        let val = 0xB000_u64 + i as u64;
        let ptr = Box::into_raw(Box::new(val)) as u64;
        let batch = Payload::handle(ptr, demo_handle_release);
        if tx.send(batch).await.is_err() {
            // Receiver gone (cursor closed): release this batch and stop.
            unsafe { demo_handle_release(ptr) };
            break;
        }
    }
}

/// Op id for a demo stream that never produces a batch — a `Next` on it parks
/// until the cursor is cancelled/closed. Used to test stream cancellation and
/// backpressure deterministically.
pub(crate) const OP_STREAM_STALL: u32 = 41;

/// A demo stream that stalls: it emits nothing and sleeps, so any `Next` parks.
pub(crate) async fn demo_stream_stall(_req: Vec<u8>, _tx: crate::BatchSender) {
    tokio::time::sleep(Duration::from_secs(3600)).await;
}

/// Dispatch a Call op to its demo handler. The op ids match the Go-side
/// constants (see examples/rust2go and the in-package tests).
pub(crate) async fn dispatch(op: u32, req: Vec<u8>) -> Result<Vec<u8>, Vec<u8>> {
    match op {
        0 => Ok(req),                                // ECHO
        1 => Ok(req.to_ascii_uppercase()),           // UPPER
        2 => Ok(req.len().to_string().into_bytes()), // LEN (decimal string)
        3 => Err(b"boom".to_vec()),                  // FAIL (error bytes)
        4 => {
            // DELAY_ECHO — a genuinely async handler (suspends on a timer),
            // proving Call runs arbitrary futures, not just sync ops.
            tokio::time::sleep(Duration::from_millis(1)).await;
            Ok(req)
        }
        5 => {
            // SLEEP_LONG — for cancellation tests: aborting drops this future.
            tokio::time::sleep(Duration::from_secs(10)).await;
            Ok(req)
        }
        10 => check_stock(req).await, // rust2go-style typed handler (examples/rust2go.md)
        _ => Err(format!("unknown op {op}").into_bytes()),
    }
}

/// A rust2go-style typed async handler: this is the Rust body that, in a real
/// rust2go project, would be an `#[rust2go::r2g]` trait method
/// (`fn check_stock(req: &Order) -> impl Future<Output = StockReply>`). Here it is
/// one op in Sable's dispatch registry. It decodes an `Order {qty, item}` from the
/// request bytes, performs a genuinely async "lookup" (a timer stands in for an
/// inventory/DB round-trip), and encodes a `StockReply {ok, message}`.
///
/// Wire format (dependency-free, mirroring rust2go's flat-struct encoding rather
/// than JSON):  request = [qty:u8][item_len:u8][item:utf8];
///              response = [ok:u8][msg_len:u8][message:utf8].
async fn check_stock(req: Vec<u8>) -> Result<Vec<u8>, Vec<u8>> {
    if req.len() < 2 {
        return Err(b"malformed Order: short".to_vec());
    }
    let qty = req[0];
    let item_len = req[1] as usize;
    if req.len() < 2 + item_len {
        return Err(b"malformed Order: truncated item".to_vec());
    }
    let item = String::from_utf8_lossy(&req[2..2 + item_len]).into_owned();

    // The async work a goroutine awaits without pinning an OS thread.
    tokio::time::sleep(Duration::from_millis(1)).await;

    const STOCK: u8 = 20; // units on hand
    let ok = qty <= STOCK;
    let message = if ok {
        format!("{item} reserved")
    } else {
        format!("only {STOCK} {item} in stock")
    };
    let msg = message.as_bytes();
    let msg = &msg[..msg.len().min(255)]; // one length byte
    let mut out = Vec::with_capacity(2 + msg.len());
    out.push(u8::from(ok));
    out.push(msg.len() as u8);
    out.extend_from_slice(msg);
    Ok(out)
}
