//! Optional real-world scenario: **reqwest** (async HTTP client) and **axum**
//! (async HTTP server) running on a SEPARATE io-enabled multi-thread tokio
//! runtime that shares sable's completion delivery.
//!
//! The core sable runtime disables tokio's IO driver so Go's netpoll is the one
//! epoll. reqwest/axum, however, use `tokio::net` directly, which REQUIRES
//! tokio's own reactor — so this runtime enables IO and has its own mio epoll
//! (two epolls in this scenario). What it demonstrates is that the *fusion*
//! layer — Go goroutines awaiting real async Rust results via gopark/goready +
//! the completion doorbell, and Rust handlers calling back into Go — works with
//! full, complex tokio applications, not just toy futures.

use std::os::raw::c_char;
use std::sync::Arc;

use tokio::runtime::Runtime;

use crate::{Inner, SableRuntime};

extern "C" {
    /// Go `//export`: called by the axum handler to compute a response in Go
    /// (Rust -> Go), demonstrating bidirectional fusion.
    fn sable_go_double(n: u64) -> u64;
}

pub struct SableHttpRuntime {
    rt: Runtime,
    inner: Arc<Inner>,
    client: reqwest::Client,
}

/// Build the http runtime, sharing the main runtime's completion plumbing.
#[unsafe(no_mangle)]
pub extern "C" fn sable_http_new(main: *const SableRuntime) -> *mut SableHttpRuntime {
    let inner = unsafe { (*main).inner() };
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(4)
        .enable_all() // io + time: reqwest/axum need tokio's reactor
        .thread_name("sable-http")
        .build()
        .expect("build http runtime");
    Box::into_raw(Box::new(SableHttpRuntime {
        rt,
        inner,
        client: reqwest::Client::new(),
    }))
}

#[unsafe(no_mangle)]
pub extern "C" fn sable_http_free(h: *mut SableHttpRuntime) {
    if !h.is_null() {
        unsafe { drop(Box::from_raw(h)) };
    }
}

/// Spawn a reqwest GET; on completion publish the response body parsed as u64
/// (or a sentinel on error) to sable's completion queue for `token`. The Go
/// awaiter is parked via gopark and resumed via goready by the shared dispatcher.
#[unsafe(no_mangle)]
pub extern "C" fn sable_http_spawn_get(
    h: *const SableHttpRuntime,
    url: *const c_char,
    len: usize,
    token: u64,
) {
    let h = unsafe { &*h };
    // Copy the URL synchronously (Go frees its buffer once this call returns).
    let bytes = unsafe { std::slice::from_raw_parts(url as *const u8, len) };
    let url = String::from_utf8_lossy(bytes).into_owned();
    let inner = h.inner.clone();
    let client = h.client.clone();
    h.rt.spawn(async move {
        let result = match client.get(&url).send().await {
            Ok(resp) => match resp.bytes().await {
                Ok(body) => std::str::from_utf8(&body)
                    .ok()
                    .and_then(|s| s.trim().parse::<u64>().ok())
                    .unwrap_or(u64::MAX - 1),
                Err(_) => u64::MAX - 2,
            },
            Err(_) => u64::MAX,
        };
        inner.publish(token, result);
    });
}

/// Start an axum server on 127.0.0.1:`port` (0 = OS-assigned). Returns the bound
/// port, or 0 on error. Routes:
///   GET /ping     -> "7"
///   GET /go?n=<k> -> "<2k>"  (computed by calling back into Go)
#[unsafe(no_mangle)]
pub extern "C" fn sable_http_start_axum(h: *const SableHttpRuntime, port: u16) -> u16 {
    use axum::{routing::get, Router};

    let h = unsafe { &*h };
    let app = Router::new()
        .route("/ping", get(|| async { "7" }))
        .route("/go", get(go_double));

    let (tx, rx) = std::sync::mpsc::channel::<u16>();
    h.rt.spawn(async move {
        match tokio::net::TcpListener::bind(("127.0.0.1", port)).await {
            Ok(listener) => {
                let bound = listener.local_addr().map(|a| a.port()).unwrap_or(0);
                let _ = tx.send(bound);
                let _ = axum::serve(listener, app).await;
            }
            Err(_) => {
                let _ = tx.send(0);
            }
        }
    });
    rx.recv().unwrap_or(0)
}

/// axum handler for `GET /go?n=<k>`: parse k, call into Go to double it
/// (Rust -> Go), return the result as the body.
async fn go_double(uri: axum::http::Uri) -> String {
    let n: u64 = uri
        .query()
        .and_then(|q| q.strip_prefix("n="))
        .and_then(|s| s.parse().ok())
        .unwrap_or(0);
    let doubled = unsafe { sable_go_double(n) };
    doubled.to_string()
}
