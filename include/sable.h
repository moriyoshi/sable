/*
 * sable.h — hand-written C ABI for the Sable Rust staticlib (Go <- Rust).
 *
 * cbindgen is intentionally NOT a build dependency (it is not installed on the
 * target box, and a hand-written header keeps the build hermetic). Keep this in
 * sync with the `#[unsafe(no_mangle)] pub extern "C"` functions in rust/src/.
 */
#ifndef SABLE_H
#define SABLE_H

#include <stdint.h>
#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Opaque runtime handle (Box<SableRuntime> on the Rust side). */
typedef struct SableRuntime SableRuntime;

/* M0 smoke test: add two integers across the FFI boundary. */
int64_t sable_add(int64_t a, int64_t b);

/* Minimal crossing for benchmarking raw Go->Rust call overhead. */
uint64_t sable_noop(uint64_t x);


/* Lifecycle. sable_runtime_new returns NULL on failure. */
SableRuntime *sable_runtime_new(void);
/* S-4.2: build on an IO-disabled MULTI-THREAD executor (workers threads, 0 =
 * num_cpus) for CPU-heavy handlers. Preserves the single-epoll invariant (see
 * s4_epoll_tests). Only linkable when the Rust `multithread` feature is built. */
SableRuntime *sable_runtime_new_multithread(int workers);
void sable_runtime_free(SableRuntime *rt);
/* Graceful shutdown WITHOUT freeing: aborts in-flight tasks + joins the executor
 * but keeps `rt` valid, so a dispatcher still referencing it can be stopped
 * before sable_runtime_free drops it (avoids a use-after-free). Idempotent. */
void sable_runtime_shutdown(const SableRuntime *rt);

/* The shared completion doorbell eventfd. Go polls this via the netpoller. */
int sable_completion_efd(const SableRuntime *rt);

/* Go awaits Rust: spawn run_demo(kind, arg); signal `token` on completion. */
void sable_spawn_await(const SableRuntime *rt, uint32_t kind, uint64_t arg, uint64_t token);

/* Go awaits (Rust awaits Go): spawn a task that awaits a Go computation,
 * signaling `token` on completion. Non-blocking; safe to fan out. */
void sable_spawn_rust_awaits_go(const SableRuntime *rt, uint32_t kind, uint64_t arg, uint64_t token);

/* Go awaits Rust doing real I/O: spawn a task that reads `nbytes` from `fd`
 * (readiness sourced from Go's netpoller), signaling the byte count on `token`. */
void sable_spawn_read_pipe(const SableRuntime *rt, int fd, uint64_t nbytes, uint64_t token);

/* Called by a Go pump goroutine when fused fd `regid` is readable. */
void sable_fd_ready(uint64_t regid);

/* Inline fast-path floor: create+poll+drop a compute future in one crossing. */
uint64_t sable_await_inline(uint32_t kind, uint64_t arg);

/* M7 inline fast path with Pending fallback. Opaque persistent future. */
typedef struct SableFuture SableFuture;
SableFuture *sable_future_new(uint32_t kind, uint64_t arg);
void sable_future_drop(SableFuture *fut);
int sable_future_poll_noop(SableFuture *fut, uint64_t *out);          /* fast: no-op waker */
int sable_future_poll(const SableRuntime *rt, SableFuture *fut, uint64_t token, uint64_t *out);

/* Pop one completed (token, result). Returns 1 if popped, 0 if the queue is
 * empty. The Go dispatcher drains-to-empty after each doorbell read. */
int sable_next_completion(const SableRuntime *rt, uint64_t *out_token, uint64_t *out_result);

/* --- Generic byte-buffer Call API (R3): arbitrary async ops; typed data is
 *     marshalled as byte buffers by the caller. Core (works portable too). --- */
void sable_call(const SableRuntime *rt, uint32_t op, const uint8_t *req, size_t req_len, uint64_t token);
void sable_call_result(uint64_t handle, int *out_ok, const uint8_t **out_ptr, size_t *out_len);
void sable_call_free(uint64_t handle);

/* --- S-2 zero-copy handle result. A registered handler (S-1) that returns
 *     Payload::Handle delivers `ptr` (opaque to sable — e.g. a
 *     *mut FFI_ArrowArrayStream) as the u64 completion result. Go takes
 *     ownership and MUST drive the pointer's own release itself, then calls
 *     sable_call_handle_taken(token) so sable does NOT double-free it. A result
 *     of 0 means "no handle" (handler errored / returned bytes); on 0 the caller
 *     must NOT call sable_call_handle_taken, and may re-run via sable_call to
 *     read the error bytes. If Go never takes the handle (shutdown), sable's
 *     release-on-drop net calls the handler's release exactly once. --- */
void sable_call_handle(const SableRuntime *rt, uint32_t op, const uint8_t *req, size_t req_len, uint64_t token);
void sable_call_handle_taken(uint64_t token);
/* Cancellable handle path: sable_call_cancel(token) aborts the in-flight op,
 * delivering 0 (the caller reports its ctx error). */
void sable_call_handle_ctx(const SableRuntime *rt, uint32_t op, const uint8_t *req, size_t req_len, uint64_t token);
/* On a 0 handle result, retrieve the handler's error WITHOUT re-running the op:
 * returns a CallResult handle (read via sable_call_result + sable_call_free) or 0
 * if the 0 was a cancellation / genuine no-handle. Removes the entry (call once). */
uint64_t sable_call_handle_error(uint64_t token);

/* Demo/test only (Rust `demo` feature): free a handle produced by the demo
 * handle op — the caller's stand-in for driving a real handle's own release. */
void sable_demo_handle_free(uint64_t ptr);

/* --- S-3 streaming Call (async get_next via repeated awaits). A registered
 *     stream handler produces batches lazily; Go pulls one per await. --- */
/* Open a stream op. Admission-controlled: returns 1 if admitted (a cursor id, or
 * 0 for an unknown op, will be delivered on `token`) or 0 if refused at the
 * in-flight cap (NO completion — caller must not park). The admission slot is held
 * for the cursor's LIFETIME, so sable_set_max_in_flight caps concurrent live
 * streams; sable_stream_close releases it. */
int sable_stream_open(const SableRuntime *rt, uint32_t op, const uint8_t *req, size_t req_len, uint64_t token);
/* Pull the next batch on `cursor`: delivers a batch handle (opaque, e.g. a
 * *mut FFI_ArrowArray) on `token`, or 0 at end-of-stream. On a non-zero result
 * Go takes ownership via sable_call_handle_taken(token), exactly as for the S-2
 * one-shot handle path. Each Next is a fresh completion (the goroutine parks). */
void sable_stream_next(const SableRuntime *rt, uint64_t cursor, uint64_t token);
/* Cancellable batch pull: sable_call_cancel(token) aborts a Next parked on a slow
 * batch, delivering the end sentinel (0). This is the race-safe way to cancel a
 * parked Next from another thread. */
void sable_stream_next_ctx(const SableRuntime *rt, uint64_t cursor, uint64_t token);
/* Close the cursor: aborts the producer (dropping the pinned stream), releases
 * buffered batches, and frees the admission slot. Safe before end-of-stream. To
 * cancel a currently-parked Next, use sable_stream_next_ctx + sable_call_cancel
 * rather than racing Close against it. */
void sable_stream_close(uint64_t cursor);

/* R4 cancellation: cancellable Call; sable_call_cancel aborts the in-flight
 * task (its Drop runs), delivering handle 0 (cancelled) to the awaiter. */
void sable_call_ctx(const SableRuntime *rt, uint32_t op, const uint8_t *req, size_t req_len, uint64_t token);
void sable_call_cancel(uint64_t token);
uint64_t sable_call_pending(void);

/* R6a observability: runtime metrics snapshot. Field order MUST match the
 * #[repr(C)] SableStats in rust/src/lib.rs and the Go mirror in go/stats.go. */
typedef struct SableStats {
    uint64_t spawned;           /* tasks admitted (one per spawn entry point) */
    uint64_t completed;         /* completions delivered (results + cancels) */
    uint64_t cancelled;         /* of `completed`, the cancellation deliveries */
    uint64_t in_flight;         /* gauge: spawned - completed */
    uint64_t queue_depth;       /* gauge: completions queued for the dispatcher */
    uint64_t peak_queue_depth;  /* high-water mark of queue_depth */
    uint64_t cancels_registered;/* cancellable calls currently registered */
    uint64_t rejected;          /* admissions refused at the in-flight cap */
    uint64_t max_in_flight;     /* current in-flight cap (0 = unbounded) */
} SableStats;
void sable_stats(const SableRuntime *rt, SableStats *out);

/* R6b backpressure: cap concurrent in-flight tasks (0 = unbounded, default).
 * sable_call_try returns 1 if admitted (a completion WILL fire on `token`) or 0
 * if refused at capacity (NO completion — caller must not park on `token`). */
void sable_set_max_in_flight(const SableRuntime *rt, uint64_t max);
int sable_call_try(const SableRuntime *rt, uint32_t op, const uint8_t *req, size_t req_len, uint64_t token);

/* --- Go-M-driven executor (goexec): tasks polled by Go worker goroutines. --- */
void sable_goexec_init(int n);              /* create N per-worker queues+doorbells */
int sable_goexec_worker_efd(int i);         /* worker i's doorbell fd (Go parks on it) */
int sable_goexec_run_worker(int i);         /* drain+run worker i's queue on this M */
/* Spawn a compute task; deliver `token` on completion (via sable_go_deliver). */
void sable_goexec_spawn(uint32_t kind, uint64_t arg, uint64_t token);

/* --- Optional real-world scenario (Rust built with `--features http`, Go with
 *     -tags sable_http): reqwest + axum on a shared-completion io-enabled
 *     runtime. These symbols only exist when the http feature is compiled. --- */
typedef struct SableHttpRuntime SableHttpRuntime;
SableHttpRuntime *sable_http_new(const SableRuntime *main_rt);
void sable_http_free(SableHttpRuntime *h);
void sable_http_spawn_get(const SableHttpRuntime *h, const char *url, size_t len, uint64_t token);
uint16_t sable_http_start_axum(const SableHttpRuntime *h, uint16_t port);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* SABLE_H */
