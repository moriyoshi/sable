//! A minimal reactor whose *source of readiness is Go's netpoller*, not mio.
//!
//! Each fused fd gets a [`Registration`]. On the Go side a "pump" goroutine
//! registers the fd with the netpoller (`poll_runtime_pollOpen`) and parks in
//! `poll_runtime_pollWait`; when the fd is readable it calls back into Rust
//! ([`on_fd_ready`], via the `sable_fd_ready` export), which flips a readiness
//! flag and fires the tokio `Waker`. The awaiting tokio task re-polls, performs
//! the non-blocking I/O, and — per the edge-triggered contract — must drain to
//! `EAGAIN` before awaiting again, which re-arms the pump.
//!
//! This is what makes Go's single epoll the one event loop for both runtimes.

use std::collections::HashMap;
use std::future::Future;
use std::os::unix::io::RawFd;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, LazyLock, Mutex};
use std::task::{Context, Poll, Waker};

// Go-side pump control (implemented as //export shims in go/poll.go). Called
// from the tokio executor thread (a foreign pthread → cgo needm).
extern "C" {
    fn sable_go_poll_start(fd: usize, regid: u64);
    fn sable_go_poll_stop(regid: u64);
}

struct Registration {
    /// Set by [`on_fd_ready`], consumed by [`Readable::poll`].
    ready: AtomicBool,
    waker: Mutex<Option<Waker>>,
}

/// regid -> Registration. Only touched (briefly) on register/deregister and by
/// the pump's readiness callback; the hot `Readable::poll` path uses the
/// `Arc<Registration>` held directly by `GoAsyncFd`, taking no global lock.
static REACTOR: LazyLock<Mutex<HashMap<u64, Arc<Registration>>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));
static NEXT_REGID: AtomicU64 = AtomicU64::new(1);

/// Called from the Go pump goroutine (via the `sable_fd_ready` export) when the
/// fd became readable. Flips readiness and wakes the task.
pub fn on_fd_ready(regid: u64) {
    // Clone the Arc out under the lock, then release before touching the reg so
    // we never hold the global lock across `wake()` (which re-enters the
    // scheduler).
    let reg = REACTOR.lock().unwrap().get(&regid).cloned();
    if let Some(reg) = reg {
        reg.ready.store(true, Ordering::Release);
        let w = reg.waker.lock().unwrap().take();
        if let Some(w) = w {
            w.wake();
        }
    }
}

/// A fused fd whose readiness is driven by Go's netpoller. Analogous to tokio's
/// `AsyncFd`, but the reactor underneath is Go's single epoll.
pub struct GoAsyncFd {
    regid: u64,
    reg: Arc<Registration>,
}

impl GoAsyncFd {
    pub fn new(fd: RawFd) -> Self {
        let regid = NEXT_REGID.fetch_add(1, Ordering::Relaxed);
        let reg = Arc::new(Registration {
            ready: AtomicBool::new(false),
            waker: Mutex::new(None),
        });
        REACTOR.lock().unwrap().insert(regid, reg.clone());
        // Start the pump goroutine; the caller keeps ownership of `fd`. Note: do
        // NOT hold the REACTOR lock across this cgo callback.
        unsafe { sable_go_poll_start(fd as usize, regid) };
        GoAsyncFd { regid, reg }
    }

    pub fn readable(&self) -> Readable<'_> {
        Readable { afd: self }
    }
}

impl Drop for GoAsyncFd {
    fn drop(&mut self) {
        unsafe { sable_go_poll_stop(self.regid) };
        REACTOR.lock().unwrap().remove(&self.regid);
    }
}

/// Future that resolves when the fd is readable (one edge).
pub struct Readable<'a> {
    afd: &'a GoAsyncFd,
}

impl<'a> Future for Readable<'a> {
    type Output = ();

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        let reg = &self.afd.reg;

        // Fast path: readiness already delivered.
        if reg.ready.swap(false, Ordering::Acquire) {
            return Poll::Ready(());
        }
        // Arm-then-check: publish the waker, then re-check readiness so a wake
        // racing between the first check and the store is not lost. The pump
        // autonomously waits for edges and fires on_fd_ready — no explicit arm.
        *reg.waker.lock().unwrap() = Some(cx.waker().clone());
        if reg.ready.swap(false, Ordering::Acquire) {
            return Poll::Ready(());
        }
        Poll::Pending
    }
}
