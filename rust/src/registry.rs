//! registry.rs — S-1: a pluggable async handler registry (the embedder-facing
//! Rust API). Today the byte-buffer Call path hardwires `demo::dispatch`; this
//! module lets a crate that *depends on sable* register `op -> async handler`
//! before `Init`, so it can host its own operations (e.g. an imbh query engine)
//! without patching sable's core.
//!
//! A handler returns a [`Payload`]: either owned **bytes** (today's path) or an
//! opaque **handle** — a raw pointer plus a release callback — that Go takes
//! ownership of on the zero-copy completion path (S-2). sable never inspects the
//! handle; it only delivers it and, if Go never takes it, drives `release` once.

use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::{Arc, LazyLock, Mutex};

/// A handler's successful payload.
pub enum Payload {
    /// Owned response bytes — the classic Call path (`sable_call` / `sable_call_result`).
    Bytes(Vec<u8>),
    /// An opaque handle Go takes ownership of on the zero-copy path (S-2). sable
    /// delivers `ptr` as the `u64` completion result and, if Go never takes it
    /// (shutdown / abort / never-taken), calls `release(ptr)` exactly once.
    /// `ptr` MUST be non-null (0 is the "no handle" sentinel on the wire).
    Handle {
        ptr: u64,
        release: unsafe extern "C" fn(u64),
    },
}

/// A handler's result: an [`Payload`] on success, or error bytes.
pub type HandlerResult = Result<Payload, Vec<u8>>;

/// A boxed, `Send` future producing a [`HandlerResult`]. Handlers must be `Send`
/// because tasks are spawned onto the executor from Go's thread (and may run on
/// a multi-threaded executor under S-4.2).
pub type HandlerFuture = Pin<Box<dyn Future<Output = HandlerResult> + Send>>;

/// An async op handler. Implemented automatically for any
/// `Fn(Vec<u8>) -> impl Future<Output = HandlerResult>` (see the blanket impl),
/// so the common case is just a closure:
///
/// ```ignore
/// sable::register(OP_QUERY, |req| async move { imbh_query_to_ffi_stream(req).await });
/// ```
pub trait AsyncHandler: Send + Sync + 'static {
    fn handle(&self, req: Vec<u8>) -> HandlerFuture;
}

impl<F, Fut> AsyncHandler for F
where
    F: Fn(Vec<u8>) -> Fut + Send + Sync + 'static,
    Fut: Future<Output = HandlerResult> + Send + 'static,
{
    fn handle(&self, req: Vec<u8>) -> HandlerFuture {
        Box::pin(self(req))
    }
}

/// The global op -> handler table. Populated by `register` before `Init`; read
/// (cloned Arc) per Call so dispatch never holds the lock across `.await`.
static REGISTRY: LazyLock<Mutex<HashMap<u32, Arc<dyn AsyncHandler>>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));

/// Register an async handler for `op`, replacing any previous one. Intended to be
/// called before `Init` (e.g. from the embedder's staticlib constructor), though
/// registration is itself thread-safe.
pub fn register(op: u32, handler: impl AsyncHandler) {
    REGISTRY.lock().unwrap().insert(op, Arc::new(handler));
}

/// Look up the handler for `op`, if any. Returns a cloned `Arc` so the caller can
/// drop the registry lock before awaiting the handler.
pub(crate) fn lookup(op: u32) -> Option<Arc<dyn AsyncHandler>> {
    REGISTRY.lock().unwrap().get(&op).cloned()
}

// ---------------------------------------------------------------------------
// S-3: streaming handlers. A stream op produces MANY batches lazily; Go pulls
// one per await (sable_stream_next). We model "async get_next" as a bounded
// channel: the handler is a producer that sends each batch as a `Payload` into
// `tx` and returns when done; sable's cursor is the matching receiver. The
// bounded channel IS the backpressure (the producer awaits capacity), and
// dropping the receiver (stream close) makes the producer's next `send` fail so
// it can stop and release the DataFusion snapshot it pinned. No `Stream` trait
// dependency, no `block_on`.
// ---------------------------------------------------------------------------

/// The channel a stream handler pushes batches into. Each item is a `Payload`
/// (typically `Payload::Handle` — one exported batch); the handler returning
/// (dropping `tx`) signals end-of-stream.
pub type BatchSender = tokio::sync::mpsc::Sender<Payload>;

/// A boxed `Send` future for a stream handler's producer body.
pub type StreamFuture = Pin<Box<dyn Future<Output = ()> + Send>>;

/// An async stream op handler. Implemented automatically for any
/// `Fn(Vec<u8>, BatchSender) -> impl Future<Output = ()>`, so the common case is
/// a producer loop:
///
/// ```ignore
/// sable::register_stream(OP_QUERY, |req, tx| async move {
///     let mut stream = imbh_query_stream(req).await;
///     while let Some(batch) = stream.next().await {
///         if tx.send(export_batch(batch)).await.is_err() { break; } // closed
///     }
/// });
/// ```
pub trait StreamHandler: Send + Sync + 'static {
    fn run(&self, req: Vec<u8>, tx: BatchSender) -> StreamFuture;
}

impl<F, Fut> StreamHandler for F
where
    F: Fn(Vec<u8>, BatchSender) -> Fut + Send + Sync + 'static,
    Fut: Future<Output = ()> + Send + 'static,
{
    fn run(&self, req: Vec<u8>, tx: BatchSender) -> StreamFuture {
        Box::pin(self(req, tx))
    }
}

static STREAM_REGISTRY: LazyLock<Mutex<HashMap<u32, Arc<dyn StreamHandler>>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));

/// Register a streaming handler for `op` (S-3), replacing any previous one.
pub fn register_stream(op: u32, handler: impl StreamHandler) {
    STREAM_REGISTRY.lock().unwrap().insert(op, Arc::new(handler));
}

/// Look up the streaming handler for `op`, if any.
pub(crate) fn lookup_stream(op: u32) -> Option<Arc<dyn StreamHandler>> {
    STREAM_REGISTRY.lock().unwrap().get(&op).cloned()
}
