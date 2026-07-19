//! Pure-tokio baseline for comparison against the sable fusion. Same workloads,
//! no Go / FFI / gopark-goready / doorbell — just native tokio spawn+await and
//! native reqwest->axum. The DIFFERENCE against the sable numbers is the cost of
//! the fusion boundary.
//!
//! To match sable fairly:
//!   * the await benchmark uses a CURRENT-THREAD runtime (sable's core executor
//!     is single-threaded), and
//!   * the HTTP benchmark uses a MULTI-THREAD runtime (as sable's http scenario).

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

fn main() {
    await_bench();
    println!();
    http_bench();
}

/// Analog of sable's "Go awaits Rust" (kind 2 = immediate echo): spawn a task
/// that returns `n`, await its result. N concurrent driver tasks pull from a
/// shared counter until `total`, mirroring sable's TestScaling.
fn await_bench() {
    println!("== pure-rust: tokio spawn+await throughput (current-thread runtime) ==");
    println!("{:<8} {:<14} {:<12}", "conc", "awaits/sec", "ns/await");
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    for conc in [1usize, 8, 64, 512, 4096, 32768] {
        let total: u64 = 200_000;
        let start = Instant::now();
        rt.block_on(async move {
            let counter = Arc::new(AtomicU64::new(0));
            let mut set = tokio::task::JoinSet::new();
            for _ in 0..conc {
                let counter = counter.clone();
                set.spawn(async move {
                    loop {
                        let n = counter.fetch_add(1, Ordering::Relaxed) + 1;
                        if n > total {
                            break;
                        }
                        let r = tokio::spawn(async move { n }).await.unwrap();
                        assert_eq!(r, n);
                    }
                });
            }
            while set.join_next().await.is_some() {}
        });
        let el = start.elapsed();
        println!(
            "{:<8} {:<14.0} {:<12.0}",
            conc,
            total as f64 / el.as_secs_f64(),
            el.as_nanos() as f64 / total as f64
        );
    }
}

/// Analog of sable's HTTP benchmark: reqwest client hitting an axum server, both
/// on a multi-thread tokio runtime. No Go in the loop.
fn http_bench() {
    println!("== pure-rust: HTTP throughput (multi-thread runtime, reqwest -> axum) ==");
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(4)
        .enable_all()
        .build()
        .unwrap();
    rt.block_on(async {
        let app = axum::Router::new().route("/ping", axum::routing::get(|| async { "7" }));
        let listener = tokio::net::TcpListener::bind(("127.0.0.1", 0)).await.unwrap();
        let port = listener.local_addr().unwrap().port();
        tokio::spawn(async move {
            let _ = axum::serve(listener, app).await;
        });
        let base = format!("http://127.0.0.1:{}", port);
        let client = reqwest::Client::new();
        for _ in 0..100 {
            if client.get(format!("{}/ping", base)).send().await.is_ok() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        for conc in [8usize, 64, 512] {
            let total: u64 = 100_000;
            let counter = Arc::new(AtomicU64::new(0));
            let start = Instant::now();
            let mut set = tokio::task::JoinSet::new();
            for _ in 0..conc {
                let (counter, client, base) = (counter.clone(), client.clone(), base.clone());
                set.spawn(async move {
                    loop {
                        let n = counter.fetch_add(1, Ordering::Relaxed) + 1;
                        if n > total {
                            break;
                        }
                        let _ = client.get(format!("{}/ping", base)).send().await;
                    }
                });
            }
            while set.join_next().await.is_some() {}
            let el = start.elapsed();
            println!(
                "conc={:<6} {:<10.0} req/sec  ({:.0} ns/req)",
                conc,
                total as f64 / el.as_secs_f64(),
                el.as_nanos() as f64 / total as f64
            );
        }
    });
}
