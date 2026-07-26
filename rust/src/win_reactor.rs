//! Windows fd-fusion reactor — the completion-model counterpart of `reactor.rs`.
//!
//! On Unix, Go's netpoller delivers *readiness edges* and the Rust task performs
//! the `read` (see `reactor::GoAsyncFd`). Windows netpoll is IOCP —
//! *completion*-based — so there is no readable edge: **Go owns the overlapped
//! read** through the runtime's single IOCP (`os.File.Read` in
//! `bridge_fusion_windows.go`) and delivers the byte count here. This module is
//! therefore a one-shot completion channel, not an edge stream: a `GoRead` future
//! suspends until Go calls [`on_read_complete`] with the count.
//!
//! Keeping Go's netpoller as the one event loop (tokio owns no IOCP) is preserved
//! exactly as on Unix; only who-performs-the-read is inverted.

use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::{Arc, LazyLock, Mutex};
use std::task::{Context, Poll, Waker};

/// Sentinel for "no result yet" (a real count is a non-negative u64 that fits i64
/// for any plausible read size).
const PENDING: i64 = -1;

struct Registration {
    /// Set by [`on_read_complete`], read by [`GoRead::poll`].
    result: AtomicI64,
    waker: Mutex<Option<Waker>>,
}

/// regid -> Registration. Touched on registration/drop and by the Go read-pump's
/// completion callback; the awaiting future holds its own `Arc<Registration>`, so
/// its poll takes no global lock.
static WIN_REACTOR: LazyLock<Mutex<HashMap<u64, Arc<Registration>>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));

/// Called from the Go read-pump goroutine (via the `sable_fd_read_complete`
/// export) when the overlapped read has finished. Stores the count and wakes the
/// task. A missing regid (task already dropped) is a harmless no-op.
pub fn on_read_complete(regid: u64, n: u64) {
    // Clone the Arc out under the lock, then release before touching the reg so we
    // never hold the global lock across `wake()` (which re-enters the scheduler) —
    // same discipline as reactor::on_fd_ready.
    let reg = WIN_REACTOR.lock().unwrap().get(&regid).cloned();
    if let Some(reg) = reg {
        reg.result.store(n as i64, Ordering::Release);
        let w = reg.waker.lock().unwrap().take();
        if let Some(w) = w {
            w.wake();
        }
    }
}

/// A future that resolves to the byte count Go's IOCP read delivered for `regid`.
/// The `Registration` is inserted at construction (BEFORE the task is spawned), so
/// a completion that races ahead of the first poll always finds the slot.
pub struct GoRead {
    regid: u64,
    reg: Arc<Registration>,
}

impl GoRead {
    pub fn new(regid: u64) -> Self {
        let reg = Arc::new(Registration {
            result: AtomicI64::new(PENDING),
            waker: Mutex::new(None),
        });
        WIN_REACTOR.lock().unwrap().insert(regid, reg.clone());
        GoRead { regid, reg }
    }
}

impl Drop for GoRead {
    fn drop(&mut self) {
        WIN_REACTOR.lock().unwrap().remove(&self.regid);
    }
}

impl Future for GoRead {
    type Output = u64;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<u64> {
        let reg = &self.reg;

        // Fast path: completion already delivered.
        let v = reg.result.load(Ordering::Acquire);
        if v != PENDING {
            return Poll::Ready(v as u64);
        }
        // Arm-then-check: publish the waker, then re-check so a completion racing
        // between the first load and the store is not lost (mirrors
        // reactor::Readable::poll).
        *reg.waker.lock().unwrap() = Some(cx.waker().clone());
        let v = reg.result.load(Ordering::Acquire);
        if v != PENDING {
            return Poll::Ready(v as u64);
        }
        Poll::Pending
    }
}
