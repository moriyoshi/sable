//! Loom model of sable's await-slot park/deliver protocol (park.go: parkCommit's
//! CAS(IDLE->PARKED) inside gopark, vs deliverCompletion's Xchg(->READY) +
//! goready). Loom exhaustively explores every interleaving of the parking
//! awaiter against the deliverer and asserts:
//!
//!   * LIVENESS: if the awaiter commits the park, it is goready'd (no lost wakeup).
//!   * EXACTLY-ONCE: goready fires at most once.
//!
//! Run with:  RUSTFLAGS="--cfg loom" cargo test --test loom_wakeup
#![cfg(loom)]

use loom::sync::atomic::{AtomicU32, Ordering};
use loom::sync::Arc;
use loom::thread;

const IDLE: u32 = 0;
const PARKED: u32 = 1;
const READY: u32 = 2;

#[test]
fn park_deliver_no_lost_wakeup() {
    loom::model(|| {
        let state = Arc::new(AtomicU32::new(IDLE));
        let wakes = Arc::new(AtomicU32::new(0)); // count of goready calls

        // Deliverer (deliverCompletion): publish result, then Xchg(READY); if the
        // awaiter was PARKED, goready it.
        let (s, w) = (state.clone(), wakes.clone());
        let deliverer = thread::spawn(move || {
            if s.swap(READY, Ordering::AcqRel) == PARKED {
                w.fetch_add(1, Ordering::Release);
            }
        });

        // Awaiter (gopark's parkCommit): commit the park only from IDLE.
        let committed = state
            .compare_exchange(IDLE, PARKED, Ordering::AcqRel, Ordering::Acquire)
            .is_ok();

        deliverer.join().unwrap();

        let woken = wakes.load(Ordering::Acquire);
        if committed {
            // Parked => must have been goready'd exactly once.
            assert_eq!(woken, 1, "committed park but goready fired {woken} times");
        } else {
            // Park aborted (state already READY) => awaiter re-checks the result,
            // and NO goready should have fired (nothing was parked).
            assert_eq!(woken, 0, "abort path but goready fired {woken} times");
        }
    });
}
