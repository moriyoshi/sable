//! Go-M-driven executor: run tokio-style tasks on **Go's M:N scheduler** instead
//! of a dedicated tokio pthread.
//!
//! `async-task` splits a future into a `Runnable` (poll once) and a `Task`
//! (handle). Each of N workers owns a `SegQueue<Runnable>` and a per-worker
//! **eventfd doorbell**; the Go side runs one worker goroutine per P, each parked
//! in Go's **netpoller** on its own doorbell fd (so the wakeup goes through Go's
//! scheduler, which is what it is tuned for — a Rust-side blocking primitive
//! instead measured ~8x worse latency). `schedule` round-robins a `Runnable` to
//! a worker's queue and rings its doorbell; that worker drains and `run()`s it.
//!
//! A task therefore *completes on a Go M*, so it delivers its result with a
//! **direct `goready`** (via `sable_go_deliver`, on the worker's own M) — no
//! completion doorbell, no dispatcher hop. Task scheduling runs across all Ps.
//!
//! Covers pure-compute futures (no tokio reactor). Futures needing tokio
//! timers/IO would run under a `Handle::enter()` guard; scheduling stays on Go.

use std::future::Future;
use std::os::raw::c_int;
use std::os::unix::io::RawFd;
use std::pin::Pin;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::OnceLock;
use std::task::{Context, Poll};

use async_task::Runnable;
use crossbeam_queue::SegQueue;

extern "C" {
    /// Go `//export`: deliver `result` for `token`. Called from within a
    /// Runnable's poll — on a Go worker goroutine's M — so its goready is legal.
    fn sable_go_deliver(token: u64, result: u64);
}

struct Worker {
    runq: SegQueue<Runnable>,
    efd: RawFd, // per-worker doorbell (EFD_NONBLOCK); no sharing => no herd
}
struct Exec {
    workers: Vec<Worker>,
    next: AtomicUsize,
}
static EXEC: OnceLock<Exec> = OnceLock::new();

fn make_eventfd() -> RawFd {
    let fd = unsafe { libc::eventfd(0, libc::EFD_NONBLOCK | libc::EFD_CLOEXEC) };
    assert!(fd >= 0, "eventfd: {}", std::io::Error::last_os_error());
    fd
}

fn schedule(runnable: Runnable) {
    let ex = EXEC.get().expect("goexec not initialized");
    let w = ex.next.fetch_add(1, Ordering::Relaxed) % ex.workers.len();
    ex.workers[w].runq.push(runnable);
    let one: u64 = 1;
    unsafe {
        libc::write(
            ex.workers[w].efd,
            &one as *const u64 as *const libc::c_void,
            8,
        )
    };
}

/// Create N workers (queues + doorbell fds). Idempotent.
#[unsafe(no_mangle)]
pub extern "C" fn sable_goexec_init(n: c_int) {
    EXEC.get_or_init(|| Exec {
        workers: (0..n.max(1))
            .map(|_| Worker {
                runq: SegQueue::new(),
                efd: make_eventfd(),
            })
            .collect(),
        next: AtomicUsize::new(0),
    });
}

/// The doorbell fd for worker `i` (Go parks on it via the netpoller).
#[unsafe(no_mangle)]
pub extern "C" fn sable_goexec_worker_efd(i: c_int) -> c_int {
    EXEC.get().unwrap().workers[i as usize].efd
}

/// Drain and run all runnables queued for worker `i` (on the calling Go M).
/// Returns the count run.
#[unsafe(no_mangle)]
pub extern "C" fn sable_goexec_run_worker(i: c_int) -> c_int {
    let w = &EXEC.get().unwrap().workers[i as usize];
    let mut c = 0;
    while let Some(r) = w.runq.pop() {
        r.run();
        c += 1;
    }
    c
}

/// A future that yields once (Pending then Ready) — exercises the re-schedule
/// path without needing tokio.
struct YieldOnce(bool);
impl Future for YieldOnce {
    type Output = ();
    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        if self.0 {
            Poll::Ready(())
        } else {
            self.0 = true;
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    }
}

async fn compute(kind: u32, arg: u64) -> u64 {
    match kind {
        2 => arg, // immediate
        3 => {
            YieldOnce(false).await;
            arg
        }
        4 => crate::cpu_work(arg), // CPU-bound — parallel across Go's Ms
        _ => 0,
    }
}

/// Spawn a compute task onto the Go-driven executor; deliver `token` on
/// completion.
#[unsafe(no_mangle)]
pub extern "C" fn sable_goexec_spawn(kind: u32, arg: u64, token: u64) {
    let fut = async move {
        let r = compute(kind, arg).await;
        unsafe { sable_go_deliver(token, r) };
    };
    let (runnable, task) = async_task::spawn(fut, schedule);
    task.detach();
    runnable.schedule();
}
