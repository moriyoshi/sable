//! Sable — fusing a Rust/tokio executor onto Go's M:N scheduler.
//!
//! # M2 — single shared epoll (Go's netpoll)
//!
//! The tokio runtime is now `new_current_thread().enable_time()` with **no**
//! `enable_io()`, driven by one dedicated executor thread doing `block_on`. With
//! no IO driver, tokio owns **no epoll** — when idle it parks on a futex/timer.
//! All fd readiness (both the completion doorbell and fused Rust I/O) flows
//! through **Go's netpoller**, so there is exactly one epoll in the process (see
//! `TestSingleEpoll`).
//!
//! * **Go awaits Rust** — unchanged from M1: task completes, pushes `(token,
//!   result)`, rings the completion doorbell eventfd; the Go dispatcher (parked
//!   in the netpoller) delivers it.
//! * **Rust awaits Go / Rust does I/O** — a tokio task uses [`reactor::GoAsyncFd`],
//!   whose readiness is delivered by a Go pump goroutine parked in
//!   `poll_runtime_pollWait`. No mio, no second epoll.

// Platform-abstracted completion doorbell (eventfd on Linux, self-pipe on other
// Unix). Core: present in every build.
mod doorbell;

// The byte-buffer Call op handlers (echo/upper/len/fail/delay/sleep/check_stock)
// exercised by the demos, examples, and test suite — separated from the runtime
// core. Gated behind the `demo` feature (on by default) so an embedder that
// owns its own handler registry (S-1) can build the runtime core WITHOUT the
// demo ops. See S-5: the binding pulls only the runtime.
#[cfg(feature = "demo")]
mod demo;

// S-1: the pluggable async handler registry an embedder populates before Init.
// Its public surface (`register`, `Payload`, `AsyncHandler`) is re-exported at
// the crate root so a dependent crate does `sable::register(op, handler)`.
mod registry;
pub use registry::{
    register, register_stream, AsyncHandler, BatchSender, Handle, HandlerFuture, HandlerResult,
    Payload, StreamFuture, StreamHandler,
};

// Real rust2go (g2r) integration: a typed Go->Rust asm crossing that kicks off
// an async task on THIS fused runtime. Opt-in (--features rust2go).
#[cfg(feature = "rust2go")]
mod r2g;

#[cfg(feature = "fast")]
mod reactor;

// Go-M-driven executor: tokio-style tasks polled on Go's M:N scheduler.
#[cfg(feature = "fast")]
mod goexec;

// Optional real-world scenario (reqwest + axum). See rust/src/http.rs.
#[cfg(feature = "http")]
mod http;

use std::collections::HashMap;
use std::collections::VecDeque;
#[cfg(feature = "fast")]
use std::future::Future;
use std::os::raw::c_int;
#[cfg(feature = "fast")]
use std::os::unix::io::RawFd;
use std::panic::{catch_unwind, AssertUnwindSafe};
#[cfg(feature = "fast")]
use std::pin::Pin;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, LazyLock, Mutex};
#[cfg(feature = "fast")]
use std::task::{Context, Poll};
use std::time::Duration;

#[cfg(feature = "fast")]
use reactor::GoAsyncFd;
// Aliased: `Handle` (the crate's public opaque-handle type, re-exported from
// registry) would otherwise collide with tokio's runtime handle.
use tokio::runtime::Handle as TokioHandle;

/// A `(token, result)` pair queued for the Go dispatcher to deliver.
type Completion = (u64, u64);

/// Shared state captured by spawned tasks so a `'static` task can ring the
/// doorbell without borrowing the runtime. `pub(crate)` so the optional `http`
/// runtime can share the same completion delivery.
pub(crate) struct Inner {
    doorbell: doorbell::Doorbell,
    completed: Mutex<VecDeque<Completion>>,
    // Observability counters (Relaxed: monotonic totals, no ordering needed).
    n_spawned: AtomicU64,
    n_completed: AtomicU64,
    n_cancelled: AtomicU64,
    peak_queue: AtomicU64,
    // Backpressure (R6b). `in_flight` is the live admit-inc / complete-dec gauge;
    // `max_in_flight` caps bounded admissions (0 = unbounded, the default);
    // `n_rejected` counts admissions refused at capacity.
    in_flight: AtomicU64,
    max_in_flight: AtomicU64,
    n_rejected: AtomicU64,
}

impl Inner {
    /// Enqueue a completion and ring the doorbell. Pure transport: the Call-style
    /// counters live in `note_complete`, called only by admitted paths, so the
    /// inline and http paths (which publish without admitting) don't skew the
    /// in-flight gauge.
    pub(crate) fn publish(&self, token: u64, result: u64) {
        let depth = {
            let mut q = self.completed.lock().unwrap();
            q.push_back((token, result));
            q.len() as u64
        };
        // Track the high-water mark of the completion backlog (a producer-faster-
        // than-consumer signal; see backpressure, R6b).
        let mut peak = self.peak_queue.load(Ordering::Relaxed);
        while depth > peak {
            match self.peak_queue.compare_exchange_weak(
                peak,
                depth,
                Ordering::Relaxed,
                Ordering::Relaxed,
            ) {
                Ok(_) => break,
                Err(p) => peak = p,
            }
        }
        self.doorbell.ring();
    }

    /// Admit a task unconditionally (the unbounded entry points). Increments the
    /// spawned total and the live in-flight gauge.
    pub(crate) fn note_spawn(&self) {
        self.n_spawned.fetch_add(1, Ordering::Relaxed);
        self.in_flight.fetch_add(1, Ordering::Relaxed);
    }

    /// Try to admit a task subject to `max_in_flight`. Returns false (counting a
    /// rejection) iff a cap is set and the live in-flight count is already at it.
    /// On success accounts the admission exactly like `note_spawn`.
    fn try_admit(&self) -> bool {
        let max = self.max_in_flight.load(Ordering::Relaxed);
        if max == 0 {
            self.note_spawn();
            return true;
        }
        let mut cur = self.in_flight.load(Ordering::Relaxed);
        loop {
            if cur >= max {
                self.n_rejected.fetch_add(1, Ordering::Relaxed);
                return false;
            }
            match self.in_flight.compare_exchange_weak(
                cur,
                cur + 1,
                Ordering::Relaxed,
                Ordering::Relaxed,
            ) {
                Ok(_) => {
                    self.n_spawned.fetch_add(1, Ordering::Relaxed);
                    return true;
                }
                Err(c) => cur = c,
            }
        }
    }

    /// Account an admitted task's completion: increments the completed total and
    /// frees its in-flight slot. Paired 1:1 with note_spawn/try_admit; called
    /// exactly once per admitted task, right where it publishes.
    fn note_complete(&self) {
        self.n_completed.fetch_add(1, Ordering::Relaxed);
        self.in_flight.fetch_sub(1, Ordering::Relaxed);
    }

    /// Complete an ADMITTED task: account it (note_complete) then publish. The
    /// inline/http paths call bare `publish` instead, so they never skew the
    /// Call-style counters.
    pub(crate) fn complete(&self, token: u64, result: u64) {
        self.note_complete();
        self.publish(token, result);
    }

    /// Count a completion that was a cancellation delivery (subset of completed).
    fn note_cancel(&self) {
        self.n_cancelled.fetch_add(1, Ordering::Relaxed);
    }
}

impl Drop for Inner {
    fn drop(&mut self) {
        // Rust owns the completion doorbell; Go polls a DUP of its read fd (see
        // sableInit), so closing here on teardown never double-closes Go's
        // descriptor. Runs when the last Arc<Inner> drops — after the executor
        // thread has joined, so no in-flight task's captured clone is still alive.
        self.doorbell.close();
    }
}

/// Opaque runtime handle handed to Go.
pub struct SableRuntime {
    handle: TokioHandle,
    inner: Arc<Inner>,
    // Shutdown plumbing (exercised in M5; unused in normal M2 runs).
    shutdown: Mutex<Option<tokio::sync::oneshot::Sender<()>>>,
    executor: Mutex<Option<std::thread::JoinHandle<()>>>,
}

impl SableRuntime {
    fn new() -> Self {
        // Current-thread runtime, TIME ONLY. No enable_io() => no mio epoll.
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_time()
            .build()
            .expect("build current-thread runtime");
        Self::from_tokio(rt)
    }

    /// S-4.2: an IO-DISABLED multi-thread executor for CPU-heavy handlers that
    /// need real parallelism. `enable_time()` but NO `enable_io()`, so no IO
    /// driver and (per s4_epoll_tests) no epoll — the single-epoll invariant
    /// holds while DataFusion-style work runs across `workers` threads.
    /// `workers == 0` lets tokio pick (num_cpus).
    #[cfg(feature = "multithread")]
    fn new_multithread(workers: usize) -> Self {
        let mut b = tokio::runtime::Builder::new_multi_thread();
        b.enable_time();
        if workers > 0 {
            b.worker_threads(workers);
        }
        let rt = b.build().expect("build multi-thread runtime");
        Self::from_tokio(rt)
    }

    /// Shared wiring: capture the runtime's Handle, build the completion `Inner`,
    /// and drive the runtime on a dedicated executor thread until shutdown. Works
    /// for either the current-thread or the multi-thread runtime.
    fn from_tokio(rt: tokio::runtime::Runtime) -> Self {
        let handle = rt.handle().clone();
        let inner = Arc::new(Inner {
            doorbell: doorbell::Doorbell::new(),
            completed: Mutex::new(VecDeque::new()),
            n_spawned: AtomicU64::new(0),
            n_completed: AtomicU64::new(0),
            n_cancelled: AtomicU64::new(0),
            peak_queue: AtomicU64::new(0),
            in_flight: AtomicU64::new(0),
            max_in_flight: AtomicU64::new(0),
            n_rejected: AtomicU64::new(0),
        });

        let (tx, rx) = tokio::sync::oneshot::channel::<()>();
        let executor = std::thread::Builder::new()
            .name("sable-executor".into())
            .spawn(move || {
                // Driving block_on runs ALL spawned tasks until shutdown. When
                // idle it parks on a condvar/timer (NOT epoll).
                rt.block_on(async move {
                    let _ = rx.await;
                });
                // `rt` drops here, on this thread — never from async context.
            })
            .expect("spawn executor thread");

        SableRuntime {
            handle,
            inner,
            shutdown: Mutex::new(Some(tx)),
            executor: Mutex::new(Some(executor)),
        }
    }

    /// Share the completion plumbing (used by the optional http runtime so its
    /// results are delivered by the same Go dispatcher).
    #[cfg(feature = "http")]
    pub(crate) fn inner(&self) -> Arc<Inner> {
        self.inner.clone()
    }
}

// ---------------------------------------------------------------------------
// eventfd helpers — used only by the fast-path "Rust awaits Go" flow, where the
// eventfd is a VALUE channel (Go writes the computed u64, Rust reads it), not a
// bare doorbell. The core completion doorbell uses the platform-abstracted
// `doorbell::Doorbell` instead (eventfd on Linux, self-pipe elsewhere).
// ---------------------------------------------------------------------------

#[cfg(feature = "fast")]
fn eventfd_nonblock() -> RawFd {
    let fd = unsafe { libc::eventfd(0, libc::EFD_NONBLOCK | libc::EFD_CLOEXEC) };
    assert!(fd >= 0, "eventfd: {}", std::io::Error::last_os_error());
    fd
}

#[cfg(feature = "fast")]
fn eventfd_read(fd: RawFd) -> u64 {
    let mut buf = [0u8; 8];
    let n = unsafe { libc::read(fd, buf.as_mut_ptr() as *mut libc::c_void, 8) };
    if n == 8 {
        u64::from_ne_bytes(buf)
    } else {
        0
    }
}

// ---------------------------------------------------------------------------
// Demo async work
// ---------------------------------------------------------------------------

/// Rust-side async work a goroutine can await (uses tokio's time driver).
async fn run_demo(kind: u32, arg: u64) -> u64 {
    match kind {
        0 => {
            tokio::time::sleep(Duration::from_millis(1)).await;
            42
        }
        1 => {
            tokio::time::sleep(Duration::from_micros(arg % 50)).await;
            arg
        }
        // kind 2: immediate echo (no timer) — for measuring raw fusion
        // round-trip throughput rather than tokio's timer granularity.
        2 => arg,
        // kind 4: CPU-bound work — serialized on the single dedicated executor
        // thread here, but parallel across Go's Ms under the goexec path.
        4 => cpu_work(arg),
        _ => 0,
    }
}

/// A fixed chunk of CPU work (~1µs) returning `arg`. `black_box` prevents the
/// loop from being optimized away.
pub(crate) fn cpu_work(arg: u64) -> u64 {
    let mut x = arg;
    for _ in 0..1000 {
        x = std::hint::black_box(
            x.wrapping_mul(2862933555777941757).wrapping_add(3037000493),
        );
    }
    std::hint::black_box(x);
    arg
}

/// A tokio task awaits a Go computation over an eventfd whose readiness is
/// delivered by **Go's netpoller** (via [`GoAsyncFd`]) — not tokio's reactor.
#[cfg(feature = "fast")]
async fn rust_awaits_go(kind: u32, arg: u64) -> u64 {
    let efd = eventfd_nonblock();
    let afd = GoAsyncFd::new(efd);
    unsafe { sable_go_compute(kind, arg, efd) };
    // Read-first, wait-on-EAGAIN (the eventfd starts at 0 = not readable).
    let raw = loop {
        let v = eventfd_read(efd);
        if v != 0 {
            break v;
        }
        afd.readable().await;
    };
    // Dropping the GoAsyncFd triggers poll_stop; the Go pump owns `efd` and
    // closes it AFTER pollClose (see poll.go) — we must not close it here, or an
    // fd-reuse race with another task's registration ensues.
    drop(afd);
    raw - 1
}

/// Read exactly `nbytes` from `fd`, sourcing readiness from Go's netpoller.
/// Honors the edge-triggered drain contract: read until `EAGAIN` before
/// awaiting readability again.
#[cfg(feature = "fast")]
async fn go_read_pipe(fd: RawFd, nbytes: usize) -> u64 {
    let afd = GoAsyncFd::new(fd);
    let mut buf = vec![0u8; nbytes];
    let mut got = 0usize;
    loop {
        // Read-first: drain everything available now.
        loop {
            let n = unsafe {
                libc::read(
                    fd,
                    buf[got..].as_mut_ptr() as *mut libc::c_void,
                    (nbytes - got) as libc::size_t,
                )
            };
            if n > 0 {
                got += n as usize;
                if got >= nbytes {
                    return got as u64;
                }
            } else if n == 0 {
                return got as u64; // writer closed early (EOF)
            } else {
                match std::io::Error::last_os_error().raw_os_error() {
                    Some(libc::EAGAIN) => break, // drained; wait for the next edge
                    Some(libc::EINTR) => continue,
                    _ => return u64::MAX,
                }
            }
        }
        // Only now, having confirmed EAGAIN, wait for readiness.
        afd.readable().await;
    }
}

// ---------------------------------------------------------------------------
// FFI: Go -> Rust
// ---------------------------------------------------------------------------

#[cfg(feature = "fast")]
extern "C" {
    /// Go `//export`: compute-and-signal for the "Rust awaits Go" direction.
    fn sable_go_compute(kind: u32, arg: u64, efd: RawFd);
}

/// M0 smoke test.
#[unsafe(no_mangle)]
pub extern "C" fn sable_add(a: i64, b: i64) -> i64 {
    a + b
}

/// Minimal crossing used to benchmark raw Go->Rust call overhead (cgo path vs
/// the asmcgocall fast path). Deliberately does nothing but return its arg.
#[unsafe(no_mangle)]
pub extern "C" fn sable_noop(x: u64) -> u64 {
    x
}

#[unsafe(no_mangle)]
pub extern "C" fn sable_runtime_new() -> *mut SableRuntime {
    catch_unwind(|| Box::into_raw(Box::new(SableRuntime::new()))).unwrap_or(std::ptr::null_mut())
}

/// S-4.2: build a runtime on an IO-disabled MULTI-THREAD executor (`workers`
/// threads, 0 = num_cpus). For CPU-heavy handlers that need parallelism without
/// giving up the single-epoll invariant. Only present with `--features multithread`.
#[cfg(feature = "multithread")]
#[unsafe(no_mangle)]
pub extern "C" fn sable_runtime_new_multithread(workers: c_int) -> *mut SableRuntime {
    let n = if workers > 0 { workers as usize } else { 0 };
    catch_unwind(|| Box::into_raw(Box::new(SableRuntime::new_multithread(n))))
        .unwrap_or(std::ptr::null_mut())
}

impl SableRuntime {
    /// Signal the executor to stop and join it. Idempotent (the tx/join handles
    /// are taken once). Joining returns only after `block_on` unwinds and the
    /// tokio Runtime drops ON THE EXECUTOR THREAD — which aborts every in-flight
    /// task (dropping their futures, running CancelGuards) and releases their
    /// captured `Arc<Inner>` clones. So after this returns, the only Inner ref
    /// left is the runtime's own.
    fn shutdown(&self) {
        let tx = self.shutdown.lock().unwrap().take();
        let j = self.executor.lock().unwrap().take();
        if let Some(tx) = tx {
            let _ = tx.send(());
        }
        if let Some(j) = j {
            let _ = j.join();
        }
        // Executor has joined -> no task can arm a new handle or buffer a new
        // batch now. Release every handle Go never took (S-2) and tear down any
        // open cursors, releasing their buffered batches (S-3).
        drain_cursors();
        drain_armed_handles();
        drain_handle_errors();
    }
}

/// Graceful shutdown WITHOUT freeing: aborts in-flight tasks and joins the
/// executor thread, but leaves the runtime box valid so a Go dispatcher that
/// still references `rt` can be stopped BEFORE `sable_runtime_free` drops it
/// (avoiding a use-after-free). Idempotent.
#[unsafe(no_mangle)]
pub extern "C" fn sable_runtime_shutdown(rt: *const SableRuntime) {
    if rt.is_null() {
        return;
    }
    let rt = unsafe { &*rt };
    let _ = catch_unwind(AssertUnwindSafe(|| rt.shutdown()));
}

#[unsafe(no_mangle)]
pub extern "C" fn sable_runtime_free(rt: *mut SableRuntime) {
    if rt.is_null() {
        return;
    }
    let _ = catch_unwind(AssertUnwindSafe(|| unsafe {
        let rt = Box::from_raw(rt);
        rt.shutdown(); // idempotent: no-op if sable_runtime_shutdown already ran
        drop(rt); // last Arc<Inner> drops here -> Inner::drop closes the eventfd
    }));
}

#[unsafe(no_mangle)]
pub extern "C" fn sable_completion_efd(rt: *const SableRuntime) -> c_int {
    let rt = unsafe { &*rt };
    rt.inner.doorbell.read_fd()
}

/// Go awaits Rust: spawn `run_demo`, signal `token` on completion.
#[unsafe(no_mangle)]
pub extern "C" fn sable_spawn_await(rt: *const SableRuntime, kind: u32, arg: u64, token: u64) {
    let rt = unsafe { &*rt };
    let inner = rt.inner.clone();
    inner.note_spawn();
    rt.handle.spawn(async move {
        let result = run_demo(kind, arg).await;
        inner.complete(token, result);
    });
}

/// Go awaits (Rust awaits Go): both directions in one flow, no thread pinned.
#[unsafe(no_mangle)]
#[cfg(feature = "fast")]
pub extern "C" fn sable_spawn_rust_awaits_go(
    rt: *const SableRuntime,
    kind: u32,
    arg: u64,
    token: u64,
) {
    let rt = unsafe { &*rt };
    let inner = rt.inner.clone();
    inner.note_spawn();
    rt.handle.spawn(async move {
        let result = rust_awaits_go(kind, arg).await;
        inner.complete(token, result);
    });
}

/// Go awaits Rust doing real I/O: spawn a task that reads `nbytes` from `fd`
/// (readiness from Go's netpoll), signaling the byte count on completion.
#[unsafe(no_mangle)]
#[cfg(feature = "fast")]
pub extern "C" fn sable_spawn_read_pipe(
    rt: *const SableRuntime,
    fd: RawFd,
    nbytes: u64,
    token: u64,
) {
    let rt = unsafe { &*rt };
    let inner = rt.inner.clone();
    inner.note_spawn();
    rt.handle.spawn(async move {
        let got = go_read_pipe(fd, nbytes as usize).await;
        inner.complete(token, got);
    });
}

/// Called by the Go pump goroutine when a fused fd is readable.
#[unsafe(no_mangle)]
#[cfg(feature = "fast")]
pub extern "C" fn sable_fd_ready(regid: u64) {
    reactor::on_fd_ready(regid);
}

/// The non-suspending "inline fast path" floor: create+poll+drop a compute future
/// on the calling M in one crossing. Returns u64::MAX on Pending (used only to
/// benchmark the floor; the real path is [`sable_future_poll_noop`] +
/// [`sable_future_poll`]).
#[unsafe(no_mangle)]
#[cfg(feature = "fast")]
pub extern "C" fn sable_await_inline(kind: u32, arg: u64) -> u64 {
    let mut fut = std::pin::pin!(run_demo(kind, arg));
    let mut cx = Context::from_waker(std::task::Waker::noop());
    match fut.as_mut().poll(&mut cx) {
        Poll::Ready(v) => v,
        Poll::Pending => u64::MAX,
    }
}

// ---------------------------------------------------------------------------
// M7: inline fast path with a Pending fallback.
//
// A persistent boxed future is polled ON THE AWAITING GOROUTINE'S M. If it is
// Ready, the value is returned with no spawn / park / cross-thread wakeup. If it
// returns Pending, an InlineWaker (keyed by a token) is installed; when it fires
// (from any thread), it publishes `token` on the shared doorbell, which the
// dispatcher turns into a goready that resumes the goroutine to re-poll.
// ---------------------------------------------------------------------------

/// Persistent future handle (opaque `SableFuture*` to Go).
#[cfg(feature = "fast")]
type InlineFut = Pin<Box<dyn Future<Output = u64> + Send>>;

/// Create a persistent future. kind 2/4 = compute (never suspends); kind 5 =
/// awaits a Go computation over Go's netpoll (suspends → exercises the fallback).
#[unsafe(no_mangle)]
#[cfg(feature = "fast")]
pub extern "C" fn sable_future_new(kind: u32, arg: u64) -> *mut InlineFut {
    let fut: InlineFut = match kind {
        5 => Box::pin(rust_awaits_go(1, arg)),
        _ => Box::pin(run_demo(kind, arg)),
    };
    Box::into_raw(Box::new(fut))
}

#[unsafe(no_mangle)]
#[cfg(feature = "fast")]
pub extern "C" fn sable_future_drop(fut: *mut InlineFut) {
    if !fut.is_null() {
        unsafe { drop(Box::from_raw(fut)) };
    }
}

/// Fast path: poll once with a no-op waker (no allocation). READY(1) sets *out.
/// Safe even for suspending futures: a wake lost to the no-op waker is caught by
/// the subsequent real-waker re-poll, which re-checks readiness.
#[unsafe(no_mangle)]
#[cfg(feature = "fast")]
pub extern "C" fn sable_future_poll_noop(fut: *mut InlineFut, out: *mut u64) -> c_int {
    let fut = unsafe { &mut *fut };
    let mut cx = Context::from_waker(std::task::Waker::noop());
    match fut.as_mut().poll(&mut cx) {
        Poll::Ready(v) => {
            unsafe { *out = v };
            1
        }
        Poll::Pending => 0,
    }
}

/// An installed waker: on wake, publish `token` on the shared doorbell so the
/// dispatcher goreadys the parked goroutine (works from any thread).
#[cfg(feature = "fast")]
struct InlineWaker {
    inner: Arc<Inner>,
    token: u64,
}
#[cfg(feature = "fast")]
impl std::task::Wake for InlineWaker {
    fn wake(self: Arc<Self>) {
        self.inner.publish(self.token, 0);
    }
    fn wake_by_ref(self: &Arc<Self>) {
        self.inner.publish(self.token, 0);
    }
}

/// Slow path: poll with a real token-keyed waker installed. READY(1) sets *out;
/// PENDING(0) means the waker will publish `token` when the future is ready.
#[unsafe(no_mangle)]
#[cfg(feature = "fast")]
pub extern "C" fn sable_future_poll(
    rt: *const SableRuntime,
    fut: *mut InlineFut,
    token: u64,
    out: *mut u64,
) -> c_int {
    let rt = unsafe { &*rt };
    let fut = unsafe { &mut *fut };
    let waker = std::task::Waker::from(Arc::new(InlineWaker {
        inner: rt.inner.clone(),
        token,
    }));
    let mut cx = Context::from_waker(&waker);
    match fut.as_mut().poll(&mut cx) {
        Poll::Ready(v) => {
            unsafe { *out = v };
            1
        }
        Poll::Pending => 0,
    }
}

/// Pop one completed `(token, result)`; 1 if popped, 0 if empty.
#[unsafe(no_mangle)]
pub extern "C" fn sable_next_completion(
    rt: *const SableRuntime,
    out_token: *mut u64,
    out_result: *mut u64,
) -> c_int {
    let rt = unsafe { &*rt };
    match rt.inner.completed.lock().unwrap().pop_front() {
        Some((t, r)) => {
            unsafe {
                *out_token = t;
                *out_result = r;
            }
            1
        }
        None => 0,
    }
}

// ---------------------------------------------------------------------------
// Generic byte-buffer Call API (R3): move off the demo-shaped u64/kind FFI to
// arbitrary async operations that marshal typed data as byte buffers. The caller
// serializes its own types (JSON/protobuf/bincode/…); the transport moves bytes
// + an ok/err flag. CORE (spawn + doorbell only), so it works in the portable
// build too. In a real library `dispatch` would be a user-populated registry of
// async handlers keyed by op id; here a few built-ins demonstrate the contract.
// ---------------------------------------------------------------------------

/// A Rust-owned result buffer handed to Go by pointer; Go copies it out, then
/// frees it via sable_call_free (single owner, single free).
pub(crate) struct CallResult {
    pub(crate) ok: bool,
    pub(crate) bytes: Vec<u8>,
}

/// S-1: dispatch a Call op through the registry, falling back to the demo ops
/// (when the `demo` feature is on) or an "unknown op" error otherwise. This is
/// the single dispatch entry both the byte path and the handle path (S-2) share,
/// so a registered handler serves either transport.
async fn dispatch(op: u32, req: Vec<u8>) -> HandlerResult {
    if let Some(h) = registry::lookup(op) {
        return h.handle(req).await;
    }
    fallback_dispatch(op, req).await
}

#[cfg(feature = "demo")]
async fn fallback_dispatch(op: u32, req: Vec<u8>) -> HandlerResult {
    // The demo handle op exercises the S-2 Payload::Handle path end-to-end; the
    // rest are the byte-returning demo ops.
    if op == demo::OP_HANDLE_DEMO {
        return Ok(demo::demo_handle(req));
    }
    demo::dispatch(op, req).await.map(Payload::Bytes)
}

#[cfg(not(feature = "demo"))]
async fn fallback_dispatch(op: u32, _req: Vec<u8>) -> HandlerResult {
    Err(format!("unknown op {op}").into_bytes())
}

/// Copy a request buffer from Go's memory into a Rust-owned `Vec` (the caller
/// keeps its buffer alive only for the duration of the FFI call via KeepAlive).
fn copy_req(req_ptr: *const u8, req_len: usize) -> Vec<u8> {
    if req_len == 0 {
        Vec::new()
    } else {
        unsafe { std::slice::from_raw_parts(req_ptr, req_len) }.to_vec()
    }
}

/// Turn a dispatch result into a boxed `CallResult` handle for the BYTE path. A
/// handler that returned `Payload::Handle` on the byte path is a misuse: release
/// the handle (so it doesn't leak) and report an error rather than smuggling a
/// raw pointer through the byte ABI.
fn byte_result_handle(r: HandlerResult) -> u64 {
    let (ok, bytes) = match r {
        Ok(Payload::Bytes(b)) => (true, b),
        Ok(Payload::Handle(_h)) => {
            // Misuse: a handle on the byte path. `_h` drops here, releasing it.
            (false, b"sable: handler returned a handle on the byte Call path".to_vec())
        }
        Err(e) => (false, e),
    };
    Box::into_raw(Box::new(CallResult { ok, bytes })) as u64
}

/// Await an arbitrary Rust async operation. Copies `req` synchronously, spawns
/// the handler, and publishes the RESULT HANDLE (a boxed CallResult pointer, as
/// u64) on completion.
#[unsafe(no_mangle)]
pub extern "C" fn sable_call(
    rt: *const SableRuntime,
    op: u32,
    req_ptr: *const u8,
    req_len: usize,
    token: u64,
) {
    let rt = unsafe { &*rt };
    let req = if req_len == 0 {
        Vec::new()
    } else {
        unsafe { std::slice::from_raw_parts(req_ptr, req_len) }.to_vec()
    };
    let inner = rt.inner.clone();
    inner.note_spawn();
    rt.handle.spawn(async move {
        let handle = byte_result_handle(dispatch(op, req).await);
        inner.complete(token, handle);
    });
}

/// Set the maximum number of concurrently in-flight tasks the `_try` admission
/// path will allow (0 = unbounded, the default). Unbounded entry points still
/// count toward the gauge but are never refused.
#[unsafe(no_mangle)]
pub extern "C" fn sable_set_max_in_flight(rt: *const SableRuntime, max: u64) {
    let rt = unsafe { &*rt };
    rt.inner.max_in_flight.store(max, Ordering::Relaxed);
}

/// Backpressure-aware sable_call. Returns 1 if admitted (a completion WILL be
/// published on `token`), or 0 if refused because the runtime is at its
/// in-flight cap (NO completion will be published — the caller must not park on
/// `token`). This is the safe primitive for a producer that can outrun the
/// runtime; the caller chooses how to react (retry, shed load, block elsewhere).
#[unsafe(no_mangle)]
pub extern "C" fn sable_call_try(
    rt: *const SableRuntime,
    op: u32,
    req_ptr: *const u8,
    req_len: usize,
    token: u64,
) -> c_int {
    let rt = unsafe { &*rt };
    if !rt.inner.try_admit() {
        return 0; // refused: no task spawned, no completion for this token
    }
    let req = if req_len == 0 {
        Vec::new()
    } else {
        unsafe { std::slice::from_raw_parts(req_ptr, req_len) }.to_vec()
    };
    let inner = rt.inner.clone();
    rt.handle.spawn(async move {
        let handle = byte_result_handle(dispatch(op, req).await);
        inner.complete(token, handle);
    });
    1
}

/// Read a completed result's (ok, ptr, len) without transferring ownership.
#[unsafe(no_mangle)]
pub extern "C" fn sable_call_result(
    handle: u64,
    out_ok: *mut c_int,
    out_ptr: *mut *const u8,
    out_len: *mut usize,
) {
    let r = unsafe { &*(handle as *const CallResult) };
    unsafe {
        *out_ok = if r.ok { 1 } else { 0 };
        // For an empty buffer, `Vec::as_ptr` returns a dangling-but-aligned sentinel (e.g. `0x1` for
        // `u8`). Handing that back would land a non-null, sub-page value in the caller's pointer slot;
        // Go's GC rejects such a value on a stack scan ("invalid pointer found on stack: 0x1"). Return a
        // null pointer when there are no bytes so the caller stores a clean 0.
        *out_ptr = if r.bytes.is_empty() {
            core::ptr::null()
        } else {
            r.bytes.as_ptr()
        };
        *out_len = r.bytes.len();
    }
}

/// Free the result buffer (after Go has copied it out).
#[unsafe(no_mangle)]
pub extern "C" fn sable_call_free(handle: u64) {
    if handle != 0 {
        unsafe { drop(Box::from_raw(handle as *mut CallResult)) };
    }
}

// ---------------------------------------------------------------------------
// S-2: zero-copy handle result. A handler that returns `Payload::Handle`
// delivers `ptr` (opaque to sable — e.g. a *mut FFI_ArrowArrayStream) as the
// bare `u64` completion result, riding the SAME (token, u64) completion path as
// everything else. Ownership model (mirrors the byte path: Rust owns, Go
// borrows the ptr, a separate call triggers the free):
//
//   * On success sable records `(token -> ArmedHandle{ptr, release})` and
//     delivers `ptr`. This is the release-on-drop safety net.
//   * Normal path: Go receives `ptr`, calls `sable_call_handle_taken(token)`
//     which drops the map entry WITHOUT calling `release` (Go now owns it), and
//     drives the pointer's own release itself. Single owner, single free.
//   * Never-taken path (shutdown): `drain_armed_handles()` calls `release(ptr)`
//     exactly once for every still-armed entry — the same publish-on-drop
//     discipline as the CancelGuard.
//
// Result encoding on the handle wire: a `u64` of 0 = "no handle" — the handler
// errored, returned bytes, or the call was cancelled. On a 0 result the caller
// must NOT call `sable_call_handle_taken`; it distinguishes the cases via
// `sable_call_handle_error` (D: error bytes recorded per token, no re-run) and,
// for the ctx variant, its own ctx.Err() (cancellation).
// ---------------------------------------------------------------------------

/// An opaque handle armed for release-on-drop. `ptr` is opaque and `release` is
/// a plain `extern "C"` fn pointer, so this is safe to move across threads.
struct ArmedHandle {
    ptr: u64,
    release: unsafe extern "C" fn(u64),
}
unsafe impl Send for ArmedHandle {}

static HANDLES: LazyLock<Mutex<HashMap<u64, ArmedHandle>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));

/// Errors produced on the handle path, keyed by token (D): a boxed `CallResult`
/// (`ok=false`) the caller reads via `sable_call_handle_error` — so a handler
/// error arrives WITHOUT re-running the op (unsafe for non-idempotent queries).
static HANDLE_ERRORS: LazyLock<Mutex<HashMap<u64, u64>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));

/// Release every still-armed handle exactly once (shutdown safety net). Called
/// from `SableRuntime::shutdown` AFTER the executor thread has joined, so no
/// in-flight task can arm a new entry concurrently.
fn drain_armed_handles() {
    let armed: Vec<ArmedHandle> = {
        let mut m = HANDLES.lock().unwrap();
        m.drain().map(|(_, v)| v).collect()
    };
    for h in armed {
        unsafe { (h.release)(h.ptr) };
    }
}

/// Free any handle-path error boxes the caller never read (shutdown safety).
fn drain_handle_errors() {
    let boxes: Vec<u64> = { HANDLE_ERRORS.lock().unwrap().drain().map(|(_, h)| h).collect() };
    for h in boxes {
        if h != 0 {
            unsafe { drop(Box::from_raw(h as *mut CallResult)) };
        }
    }
}

/// Record error bytes for `token` on the handle path as a boxed `CallResult`.
fn record_handle_error(token: u64, bytes: Vec<u8>) {
    let h = Box::into_raw(Box::new(CallResult { ok: false, bytes })) as u64;
    HANDLE_ERRORS.lock().unwrap().insert(token, h);
}

/// Deliver a dispatch result on the handle path (shared by the plain and ctx
/// entry points): arm + deliver a real handle, or record an error box and deliver
/// 0. A null-ptr handle and an end/bytes result deliver 0 with no error.
fn deliver_handle_result(inner: &Inner, token: u64, r: HandlerResult) {
    match r {
        Ok(Payload::Handle(h)) if h.ptr() != 0 => {
            // Arm the net BEFORE completing so a shutdown racing delivery still
            // frees it; `handle_taken` disarms on the normal path.
            let (ptr, release) = h.into_raw();
            HANDLES.lock().unwrap().insert(token, ArmedHandle { ptr, release });
            inner.complete(token, ptr);
        }
        Ok(Payload::Handle(_h)) => inner.complete(token, 0), // null ptr: _h drops
        Ok(Payload::Bytes(_)) => {
            record_handle_error(token, b"sable: handler returned bytes on the handle path".to_vec());
            inner.complete(token, 0);
        }
        Err(e) => {
            record_handle_error(token, e);
            inner.complete(token, 0);
        }
    }
}

/// Await a Call op on the zero-copy handle path. On success arms the release net
/// and delivers `ptr`; on error records the error (see `sable_call_handle_error`)
/// and delivers 0. Not cancellable — use `sable_call_handle_ctx` for that.
#[unsafe(no_mangle)]
pub extern "C" fn sable_call_handle(
    rt: *const SableRuntime,
    op: u32,
    req_ptr: *const u8,
    req_len: usize,
    token: u64,
) {
    let rt = unsafe { &*rt };
    let req = copy_req(req_ptr, req_len);
    let inner = rt.inner.clone();
    inner.note_spawn();
    rt.handle.spawn(async move {
        let r = dispatch(op, req).await;
        deliver_handle_result(&inner, token, r);
    });
}

/// Cancellable handle path (A): like `sable_call_handle`, but registers an
/// AbortHandle so `sable_call_cancel(token)` aborts the in-flight op (its future
/// dropped, its `Drop` run), delivering 0 (the caller reports ctx.Err()).
#[unsafe(no_mangle)]
pub extern "C" fn sable_call_handle_ctx(
    rt: *const SableRuntime,
    op: u32,
    req_ptr: *const u8,
    req_len: usize,
    token: u64,
) {
    let rt = unsafe { &*rt };
    let req = copy_req(req_ptr, req_len);
    let inner = rt.inner.clone();
    let cell: CancelCell = Arc::new(Mutex::new(None));
    CANCELS.lock().unwrap().insert(token, cell.clone());
    inner.note_spawn();
    let guard = CancelGuard { inner: inner.clone(), token, armed: true };
    let jh = rt.handle.spawn(async move {
        let mut guard = guard;
        let r = dispatch(op, req).await;
        guard.armed = false; // completed normally
        deliver_handle_result(&inner, token, r);
        // guard drops here (disarmed): removes from CANCELS, no cancel publish.
    });
    *cell.lock().unwrap() = Some(jh.abort_handle());
}

/// Retrieve the error recorded for a 0-result handle call (D). Returns a
/// `CallResult` handle (read via `sable_call_result` + `sable_call_free`, exactly
/// like the byte path) or 0 if there was none (cancellation, or a genuine "no
/// handle"). Removes the entry — call at most once per token.
#[unsafe(no_mangle)]
pub extern "C" fn sable_call_handle_error(token: u64) -> u64 {
    HANDLE_ERRORS.lock().unwrap().remove(&token).unwrap_or(0)
}

/// Tell sable Go has taken ownership of the handle delivered on `token`: drop the
/// armed entry WITHOUT releasing (Go now drives the free). A no-op for an unknown
/// token (already taken, or delivered as a 0 "no handle"). Shared by the S-2
/// one-shot handle path and the S-3 per-batch stream path.
#[unsafe(no_mangle)]
pub extern "C" fn sable_call_handle_taken(token: u64) {
    let _ = HANDLES.lock().unwrap().remove(&token);
}

// ---------------------------------------------------------------------------
// S-3: streaming Call (async get_next via repeated awaits). Iteration is modeled
// as a server-side cursor over a bounded channel:
//
//   * sable_stream_open runs the stream handler as a PRODUCER task that pushes
//     each batch (a Payload) into a bounded mpsc, stores the receiver in a cursor
//     registry, and delivers the cursor id (u64) on the token.
//   * sable_stream_next spawns a fresh task that awaits ONE batch from the
//     cursor's channel and completes the token with the batch handle (0 =
//     end-of-stream). Each Next is an ordinary single completion — the goroutine
//     parks, no M is blocked, no block_on.
//   * sable_stream_close aborts the producer (dropping the DataFusion stream and
//     its pinned snapshot) and drains any buffered batch handles so they don't
//     leak. The bounded channel provides backpressure for free.
//
// Per-batch handles reuse the S-2 release net (HANDLES) + sable_call_handle_taken.
// ---------------------------------------------------------------------------

/// Bounded channel depth: how many produced-but-unpulled batches sable buffers
/// before the producer awaits capacity (backpressure).
const STREAM_BUF: usize = 4;

/// A live server-side cursor: the receiving end of the producer's batch channel,
/// a handle to abort the producer, and the runtime `Inner` so `close`/teardown can
/// release the admission slot held for the cursor's lifetime (C).
struct Cursor {
    rx: Arc<tokio::sync::Mutex<tokio::sync::mpsc::Receiver<Payload>>>,
    producer: tokio::task::AbortHandle,
    inner: Arc<Inner>,
}

static CURSORS: LazyLock<Mutex<HashMap<u64, Cursor>>> = LazyLock::new(|| Mutex::new(HashMap::new()));
static CURSOR_CTR: AtomicU64 = AtomicU64::new(1);

/// Per-`Next` cancel guard (A). Unlike `CancelGuard`, the stream path does not use
/// the Call admission gauge (the cursor holds the slot), so on abort it delivers
/// the end sentinel via `publish` rather than `complete`.
struct StreamCancelGuard {
    inner: Arc<Inner>,
    token: u64,
    armed: bool,
}
impl Drop for StreamCancelGuard {
    fn drop(&mut self) {
        CANCELS.lock().unwrap().remove(&self.token);
        if self.armed {
            self.inner.note_cancel();
            self.inner.publish(self.token, 0); // cancelled => end-of-stream sentinel
        }
    }
}

/// Resolve a stream op to its handler: registry first, then the demo stream op.
fn stream_handler(op: u32) -> Option<Arc<dyn StreamHandler>> {
    if let Some(h) = registry::lookup_stream(op) {
        return Some(h);
    }
    #[cfg(feature = "demo")]
    if op == demo::OP_STREAM_DEMO {
        return Some(Arc::new(demo::demo_stream) as Arc<dyn StreamHandler>);
    }
    #[cfg(feature = "demo")]
    if op == demo::OP_STREAM_STALL {
        return Some(Arc::new(demo::demo_stream_stall) as Arc<dyn StreamHandler>);
    }
    None
}

/// Deliver one pulled batch on `token`: arm + publish a real handle, else publish
/// 0 (null handle / bytes / end-of-stream). A dropped `Payload` releases its
/// handle (E), so the non-handle arms leak nothing. Uses `publish`, not
/// `complete`: batch pulls don't touch the Call admission gauge (the cursor does).
fn deliver_batch(inner: &Inner, token: u64, batch: Option<Payload>) {
    match batch {
        Some(Payload::Handle(h)) if h.ptr() != 0 => {
            let (ptr, release) = h.into_raw();
            HANDLES.lock().unwrap().insert(token, ArmedHandle { ptr, release });
            inner.publish(token, ptr);
        }
        _ => inner.publish(token, 0),
    }
}

/// Tear a cursor down: abort the producer (dropping the pinned DataFusion stream)
/// and drain buffered batches — each dropped `Payload` releases its handle (E).
/// Best-effort on the lock: an in-flight `Next` owns the batch it pulled.
fn drain_cursor(c: &Cursor) {
    c.producer.abort();
    if let Ok(mut rx) = c.rx.try_lock() {
        while rx.try_recv().is_ok() {} // dropping each Payload releases its handle
    }
}

/// Release-net for cursors Go never closed (shutdown safety, mirrors
/// `drain_armed_handles`). Called after the executor has joined.
fn drain_cursors() {
    let cursors: Vec<Cursor> = {
        let mut m = CURSORS.lock().unwrap();
        m.drain().map(|(_, c)| c).collect()
    };
    for c in cursors {
        drain_cursor(&c);
        c.inner.note_complete(); // release the admission slot held since open (C)
    }
}

/// Register a producer + cursor for an admitted open, deep inside the open task.
/// Returns the cursor id, or releases the slot and returns 0 for an unknown op.
async fn open_cursor(inner: &Arc<Inner>, op: u32, req: Vec<u8>) -> u64 {
    let handler = match stream_handler(op) {
        Some(h) => h,
        None => {
            inner.note_complete(); // release the slot; no cursor created
            return 0;
        }
    };
    let (tx, rx) = tokio::sync::mpsc::channel::<Payload>(STREAM_BUF);
    // Producer runs on this runtime; dropping tx (return) ends the stream.
    let producer = tokio::spawn(async move { handler.run(req, tx).await });
    let cursor_id = CURSOR_CTR.fetch_add(1, Ordering::Relaxed);
    CURSORS.lock().unwrap().insert(
        cursor_id,
        Cursor {
            rx: Arc::new(tokio::sync::Mutex::new(rx)),
            producer: producer.abort_handle(),
            inner: inner.clone(),
        },
    );
    cursor_id
}

/// Open a streaming op. Admission-controlled (C): returns 1 if admitted (a cursor
/// id — or 0 for an unknown op — will be delivered on `token`) or 0 if refused at
/// the in-flight cap (NO completion; caller must not park). The admission slot is
/// held for the cursor's LIFETIME, so `SetMaxInFlight` caps concurrent live
/// streams; `sable_stream_close` releases it.
#[unsafe(no_mangle)]
pub extern "C" fn sable_stream_open(
    rt: *const SableRuntime,
    op: u32,
    req_ptr: *const u8,
    req_len: usize,
    token: u64,
) -> c_int {
    let rt = unsafe { &*rt };
    if !rt.inner.try_admit() {
        return 0; // refused at cap: no cursor, caller must not park on `token`
    }
    let req = copy_req(req_ptr, req_len);
    let inner = rt.inner.clone();
    rt.handle.spawn(async move {
        let cursor_id = open_cursor(&inner, op, req).await;
        // Deliver WITHOUT note_complete: the slot is held until close (C).
        inner.publish(token, cursor_id);
    });
    1
}

/// Pull the next batch on `cursor` (non-cancellable): delivers its handle on
/// `token`, or 0 at end-of-stream / unknown cursor. A fresh completion per call —
/// the goroutine parks, no M blocks, no `block_on`.
#[unsafe(no_mangle)]
pub extern "C" fn sable_stream_next(rt: *const SableRuntime, cursor: u64, token: u64) {
    let rt = unsafe { &*rt };
    let inner = rt.inner.clone();
    let rx = CURSORS.lock().unwrap().get(&cursor).map(|c| c.rx.clone());
    let rx = match rx {
        Some(r) => r,
        None => {
            inner.publish(token, 0); // unknown/closed cursor => end
            return;
        }
    };
    rt.handle.spawn(async move {
        let batch = {
            let mut guard = rx.lock().await;
            guard.recv().await
        };
        deliver_batch(&inner, token, batch);
    });
}

/// Cancellable batch pull (A): like `sable_stream_next`, but registers an
/// AbortHandle so `sable_call_cancel(token)` aborts a `Next` parked on a slow
/// batch — dropping the recv future (releasing the rx lock) and delivering the end
/// sentinel. This is the race-safe mid-stream cancel (a parked `Next` can be
/// cancelled from another goroutine); `Close` remains for teardown.
#[unsafe(no_mangle)]
pub extern "C" fn sable_stream_next_ctx(rt: *const SableRuntime, cursor: u64, token: u64) {
    let rt = unsafe { &*rt };
    let inner = rt.inner.clone();
    let rx = CURSORS.lock().unwrap().get(&cursor).map(|c| c.rx.clone());
    let rx = match rx {
        Some(r) => r,
        None => {
            inner.publish(token, 0);
            return;
        }
    };
    let cell: CancelCell = Arc::new(Mutex::new(None));
    CANCELS.lock().unwrap().insert(token, cell.clone());
    let guard = StreamCancelGuard { inner: inner.clone(), token, armed: true };
    let jh = rt.handle.spawn(async move {
        let mut guard = guard;
        let batch = {
            let mut g = rx.lock().await;
            g.recv().await
        };
        guard.armed = false; // received (or ended) normally, not cancelled
        deliver_batch(&inner, token, batch);
    });
    *cell.lock().unwrap() = Some(jh.abort_handle());
}

/// Close a cursor: abort the producer (dropping the pinned DataFusion stream),
/// release buffered batches, and free the admission slot held since open (C). Safe
/// before end-of-stream (cancel of a whole stream). To cancel a `Next` that is
/// currently parked, use `sable_stream_next_ctx` + `sable_call_cancel` instead of
/// racing `Close` against it.
#[unsafe(no_mangle)]
pub extern "C" fn sable_stream_close(cursor: u64) {
    let c = CURSORS.lock().unwrap().remove(&cursor);
    if let Some(c) = c {
        drain_cursor(&c);
        c.inner.note_complete(); // release the admission slot (C)
    }
}

// ---------------------------------------------------------------------------
// R4: cancellation & lifecycle. A CancelCell holds the task's AbortHandle,
// inserted into CANCELS *before* the spawn so a fast-completing task can't
// orphan the map entry. A CancelGuard on the task delivers EXACTLY ONE
// completion: on normal finish it disarms and publishes the real result; if the
// future is dropped first (abort / runtime shutdown) it publishes a cancellation
// (handle 0) so the awaiter unparks, and removes itself from CANCELS. Aborting a
// suspended future runs its Drop, releasing whatever it held (fds, etc.).
// ---------------------------------------------------------------------------

type CancelCell = Arc<Mutex<Option<tokio::task::AbortHandle>>>;
static CANCELS: LazyLock<Mutex<HashMap<u64, CancelCell>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));

struct CancelGuard {
    inner: Arc<Inner>,
    token: u64,
    armed: bool,
}
impl Drop for CancelGuard {
    fn drop(&mut self) {
        CANCELS.lock().unwrap().remove(&self.token);
        if self.armed {
            // Dropped before completing (aborted) -> deliver a cancellation.
            self.inner.note_cancel();
            self.inner.complete(self.token, 0);
        }
    }
}

/// Like sable_call, but cancellable: register an AbortHandle so sable_call_cancel
/// can drop the future. Result handle is published on normal completion; a
/// cancellation (handle 0) is published if the task is aborted.
#[unsafe(no_mangle)]
pub extern "C" fn sable_call_ctx(
    rt: *const SableRuntime,
    op: u32,
    req_ptr: *const u8,
    req_len: usize,
    token: u64,
) {
    let rt = unsafe { &*rt };
    let req = if req_len == 0 {
        Vec::new()
    } else {
        unsafe { std::slice::from_raw_parts(req_ptr, req_len) }.to_vec()
    };
    let inner = rt.inner.clone();

    // Insert the (empty) cell BEFORE spawning so the task's guard can't remove a
    // not-yet-present entry (which would orphan the fill below).
    let cell: CancelCell = Arc::new(Mutex::new(None));
    CANCELS.lock().unwrap().insert(token, cell.clone());
    inner.note_spawn();

    // Create the guard OUTSIDE the async block so it is captured by the future:
    // its Drop then runs even if the task is aborted BEFORE its first poll (in
    // which case the body — and any guard created inside it — would never run).
    let guard = CancelGuard {
        inner: inner.clone(),
        token,
        armed: true,
    };
    let jh = rt.handle.spawn(async move {
        let mut guard = guard;
        let handle = byte_result_handle(dispatch(op, req).await);
        guard.armed = false; // completed normally
        inner.complete(token, handle);
        // guard drops here (disarmed): removes from CANCELS, no cancel publish.
    });
    *cell.lock().unwrap() = Some(jh.abort_handle());
}

/// Abort an in-flight sable_call_ctx task (best-effort — a no-op if it already
/// finished). Dropping the future runs its Drop + the CancelGuard, which
/// publishes the cancellation.
#[unsafe(no_mangle)]
pub extern "C" fn sable_call_cancel(token: u64) {
    let cell = CANCELS.lock().unwrap().get(&token).cloned();
    if let Some(cell) = cell {
        if let Some(h) = cell.lock().unwrap().as_ref() {
            h.abort();
        }
    }
}

/// Debug: number of in-flight cancellable calls (should drain to 0).
#[unsafe(no_mangle)]
pub extern "C" fn sable_call_pending() -> u64 {
    CANCELS.lock().unwrap().len() as u64
}

#[cfg(all(test, feature = "multithread", target_os = "linux"))]
mod s4_epoll_tests {
    //! S-4.2 empirical validation: does an IO-disabled multi-thread tokio
    //! runtime create an epoll? The prescription's whole premise is that the
    //! epoll comes from the IO driver (`enable_io`), NOT from the multi-thread
    //! scheduler (whose workers park on a condvar). If that holds, sable can
    //! offer real parallelism for DataFusion-style handlers while keeping the
    //! single-epoll invariant. We mirror `countEpollFds` (Go side) by counting
    //! `[eventpoll]` links in /proc/self/fd.
    use super::*;
    use std::time::Duration;

    fn count_eventpoll() -> usize {
        let dir = match std::fs::read_dir("/proc/self/fd") {
            Ok(d) => d,
            Err(_) => return usize::MAX,
        };
        dir.filter_map(|e| e.ok())
            .filter(|e| {
                std::fs::read_link(e.path())
                    .map(|p| p.to_string_lossy().contains("[eventpoll]"))
                    .unwrap_or(false)
            })
            .count()
    }

    #[test]
    fn multithread_time_only_creates_no_epoll() {
        let base = count_eventpoll();
        let sable = SableRuntime::new_multithread(4);
        // Actually run time-based work across the workers, in case any epoll
        // were created lazily on first poll rather than at build.
        for _ in 0..16 {
            sable.handle.spawn(async {
                tokio::time::sleep(Duration::from_millis(2)).await;
            });
        }
        std::thread::sleep(Duration::from_millis(60));
        let after = count_eventpoll();
        sable.shutdown();
        assert_eq!(
            after, base,
            "multi_thread().enable_time() (no enable_io) must create NO epoll; \
             base={base} after={after}"
        );
    }

    #[test]
    fn control_enable_all_does_create_an_epoll() {
        // Proves the detector works AND that the epoll is IO-driver-sourced: the
        // ONLY difference from the test above is enable_all (= + enable_io) plus
        // an actual IO resource. tokio creates the epoll LAZILY on first IO use,
        // so we must bind a socket to force the IO driver to materialize it.
        let base = count_eventpoll();
        let rt = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .expect("build enable_all runtime");
        let after = rt.block_on(async {
            let _l = tokio::net::TcpListener::bind(("127.0.0.1", 0))
                .await
                .expect("bind forces the IO driver's epoll");
            count_eventpoll()
        });
        assert!(
            after > base,
            "enable_all (io+time) + a bound socket must create >=1 epoll (control); \
             base={base} after={after}"
        );
        drop(rt);
    }
}

#[cfg(test)]
mod registry_tests {
    //! S-1/S-2: the registry replaces demo dispatch, and a handler can return
    //! either bytes or an opaque handle. Exercises the shared `dispatch` fn.
    use super::*;

    #[tokio::test]
    async fn registered_bytes_handler_overrides_dispatch() {
        register(9_001, |req: Vec<u8>| async move {
            Ok(Payload::Bytes(req.iter().rev().copied().collect()))
        });
        match dispatch(9_001, vec![1, 2, 3]).await {
            Ok(Payload::Bytes(b)) => assert_eq!(b, vec![3, 2, 1]),
            _ => panic!("expected reversed bytes"),
        }
    }

    #[tokio::test]
    async fn registered_handler_returns_opaque_handle() {
        unsafe extern "C" fn rel(p: u64) {
            drop(unsafe { Box::from_raw(p as *mut u64) });
        }
        register(9_002, |_req| async move {
            Ok(Payload::handle(Box::into_raw(Box::new(7u64)) as u64, rel))
        });
        match dispatch(9_002, Vec::new()).await {
            Ok(Payload::Handle(h)) => {
                assert_eq!(unsafe { *(h.ptr() as *const u64) }, 7);
                let (ptr, release) = h.into_raw();
                unsafe { release(ptr) }; // no leak (Miri/valgrind)
            }
            _ => panic!("expected handle"),
        }
    }

    #[tokio::test]
    async fn registered_stream_handler_produces_batches() {
        // S-3: a stream handler pushes batches into the channel; the receiver
        // (sable's cursor) pulls them in order until the producer returns.
        register_stream(9_100, |req: Vec<u8>, tx: crate::BatchSender| async move {
            let n = req.first().copied().unwrap_or(0);
            for i in 0..n {
                if tx.send(Payload::Bytes(vec![i])).await.is_err() {
                    break;
                }
            }
        });
        let handler = registry::lookup_stream(9_100).expect("registered");
        let (tx, mut rx) = tokio::sync::mpsc::channel::<Payload>(4);
        handler.run(vec![3], tx).await;
        let mut seen = Vec::new();
        while let Some(Payload::Bytes(b)) = rx.recv().await {
            seen.push(b[0]);
        }
        assert_eq!(seen, vec![0, 1, 2]);
    }

    #[test]
    fn byte_path_releases_a_misused_handle() {
        // A handler that returns a Handle on the BYTE path: byte_result_handle
        // must release it (not leak) and report an error.
        unsafe extern "C" fn rel(p: u64) {
            drop(unsafe { Box::from_raw(p as *mut u64) });
        }
        let h = byte_result_handle(Ok(Payload::handle(Box::into_raw(Box::new(1u64)) as u64, rel)));
        let r = unsafe { &*(h as *const CallResult) };
        assert!(!r.ok);
        sable_call_free(h);
    }
}

#[cfg(test)]
mod call_unsafe_tests {
    //! Miri target: the raw-pointer lifecycle behind the byte-buffer Call ABI
    //! (Box::into_raw -> read via sable_call_result -> free via sable_call_free).
    //! Run under UB checking: cargo +nightly miri test --no-default-features
    use super::*;

    #[test]
    fn call_result_roundtrip_and_free() {
        let handle = Box::into_raw(Box::new(CallResult { ok: true, bytes: vec![1u8, 2, 3, 0, 255] })) as u64;
        let mut ok: std::os::raw::c_int = -1;
        let mut ptr: *const u8 = std::ptr::null();
        let mut len: usize = 0;
        sable_call_result(handle, &mut ok, &mut ptr, &mut len);
        assert_eq!(ok, 1);
        assert_eq!(len, 5);
        let seen = unsafe { std::slice::from_raw_parts(ptr, len) };
        assert_eq!(seen, &[1u8, 2, 3, 0, 255]);
        sable_call_free(handle); // must not leak / double-free (Miri checks)
    }

    #[test]
    fn call_result_err_flag() {
        let handle = Box::into_raw(Box::new(CallResult { ok: false, bytes: b"boom".to_vec() })) as u64;
        let mut ok: std::os::raw::c_int = -1;
        let mut ptr: *const u8 = std::ptr::null();
        let mut len: usize = 0;
        sable_call_result(handle, &mut ok, &mut ptr, &mut len);
        assert_eq!(ok, 0);
        assert_eq!(unsafe { std::slice::from_raw_parts(ptr, len) }, b"boom");
        sable_call_free(handle);
    }

    #[test]
    fn call_free_null_is_noop() {
        sable_call_free(0); // handle 0 = sentinel; must be a safe no-op
    }

    #[test]
    fn empty_result_returns_null_ptr() {
        // An empty `Vec<u8>` yields the dangling `Vec::as_ptr` sentinel (0x1 for u8).
        // sable_call_result must map that to a NULL out-pointer so the Go caller never
        // parks a sub-page value in a pointer slot ("invalid pointer found on stack").
        let handle = Box::into_raw(Box::new(CallResult { ok: true, bytes: Vec::new() })) as u64;
        let mut ok: std::os::raw::c_int = -1;
        let mut ptr: *const u8 = 0x1 as *const u8; // pre-seed with a non-null sentinel
        let mut len: usize = 999;
        sable_call_result(handle, &mut ok, &mut ptr, &mut len);
        assert_eq!(ok, 1);
        assert_eq!(len, 0);
        assert!(ptr.is_null(), "empty ok result must yield a NULL ptr, got {ptr:p}");
        sable_call_free(handle);
    }

    #[test]
    fn empty_err_result_returns_null_ptr() {
        // Same guarantee on the error branch: an empty error buffer must also null the ptr.
        let handle = Box::into_raw(Box::new(CallResult { ok: false, bytes: Vec::new() })) as u64;
        let mut ok: std::os::raw::c_int = -1;
        let mut ptr: *const u8 = 0x1 as *const u8;
        let mut len: usize = 999;
        sable_call_result(handle, &mut ok, &mut ptr, &mut len);
        assert_eq!(ok, 0);
        assert_eq!(len, 0);
        assert!(ptr.is_null(), "empty err result must yield a NULL ptr, got {ptr:p}");
        sable_call_free(handle);
    }

    #[test]
    fn nonempty_result_returns_valid_ptr() {
        // The non-empty path must still hand back the live buffer pointer unchanged.
        let handle = Box::into_raw(Box::new(CallResult { ok: true, bytes: vec![7u8] })) as u64;
        let mut ok: std::os::raw::c_int = -1;
        let mut ptr: *const u8 = std::ptr::null();
        let mut len: usize = 0;
        sable_call_result(handle, &mut ok, &mut ptr, &mut len);
        assert_eq!(ok, 1);
        assert_eq!(len, 1);
        assert!(!ptr.is_null(), "non-empty result must yield a live ptr");
        assert_eq!(unsafe { std::slice::from_raw_parts(ptr, len) }, &[7u8]);
        sable_call_free(handle);
    }
}

// ---------------------------------------------------------------------------
// R6a: observability. A snapshot of runtime counters for metrics/pprof export.
// ---------------------------------------------------------------------------

/// Runtime metrics snapshot (repr(C): mirrored field-for-field in sable.h and
/// go/stats.go). All monotonic totals except the gauges (in_flight, queue_depth).
#[repr(C)]
pub struct SableStats {
    /// Tasks admitted to the runtime (one per spawn entry point).
    pub spawned: u64,
    /// Completions delivered to Go (normal results + cancellation deliveries).
    pub completed: u64,
    /// Of `completed`, the count that were cancellation deliveries.
    pub cancelled: u64,
    /// Gauge: tasks spawned but not yet delivered (spawned - completed).
    pub in_flight: u64,
    /// Gauge: completions queued right now, awaiting the Go dispatcher drain.
    pub queue_depth: u64,
    /// High-water mark of `queue_depth` (a producer-outpacing-consumer signal).
    pub peak_queue_depth: u64,
    /// Cancellable calls currently registered (should track in-flight ctx calls).
    pub cancels_registered: u64,
    /// Admissions refused by the `_try` path at the in-flight cap (monotonic).
    pub rejected: u64,
    /// The current in-flight cap (0 = unbounded).
    pub max_in_flight: u64,
}

/// Fill `*out` with a metrics snapshot. Lock-free except a brief lock to read
/// the live queue depth and the cancel registry size.
#[unsafe(no_mangle)]
pub extern "C" fn sable_stats(rt: *const SableRuntime, out: *mut SableStats) {
    let rt = unsafe { &*rt };
    let i = &rt.inner;
    let queue_depth = i.completed.lock().unwrap().len() as u64;
    let cancels_registered = CANCELS.lock().unwrap().len() as u64;
    let snap = SableStats {
        spawned: i.n_spawned.load(Ordering::Relaxed),
        completed: i.n_completed.load(Ordering::Relaxed),
        cancelled: i.n_cancelled.load(Ordering::Relaxed),
        in_flight: i.in_flight.load(Ordering::Relaxed),
        queue_depth,
        peak_queue_depth: i.peak_queue.load(Ordering::Relaxed),
        cancels_registered,
        rejected: i.n_rejected.load(Ordering::Relaxed),
        max_in_flight: i.max_in_flight.load(Ordering::Relaxed),
    };
    unsafe { *out = snap };
}
