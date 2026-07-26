# Sable — work journal

A development log of the project and the findings behind it, in roughly the order
work happened. The recent session (productionization onward) is first-hand; the
earlier milestones are reconstructed from persistent memory, the README, and the
original design plan.

All build variants stay green throughout: default `-race`, `sable_safe`,
`sable_portable`, `sable_http`, `sable_rust2go`, plus `abi-check`, `loom`, `fuzz`.

## The idea (context)

Fuse a Rust **tokio** runtime with Go's **M:N (GMP)** scheduler into **one shared
OS event loop**, with **symmetric mutual await**: a goroutine can `await` a Rust
async result and a tokio task can `await` a Go result, each without a blocked OS
thread and without busy-polling. No prior work fuses these two at the scheduler
level (rust2go keeps two reactors; UniFFI polls from the foreign side but tokio
still runs its own reactor; go-lib reimplements GMP in Rust).

**The enabling trick.** Stock tokio hides its `mio` epoll and offers no manual
reactor turn, so instead of forking tokio, Sable **removes tokio's reactor**: the
runtime is `new_current_thread().enable_time()` with **no `enable_io()`**, driven
by one executor thread doing `block_on`. With no IO driver tokio owns **zero
epoll** (parks on a futex); **Go's netpoll becomes the only epoll in the process**
(asserted by `TestSingleEpoll` scanning `/proc/self/fd`). tokio still provides its
scheduler, `Waker`, `spawn`, and combinators.

**The correctness lynchpin (shaped everything below).** `gopark`/`goready` are
legal only on a real Go M. The Rust executor thread is a raw pthread — it must
**never** call `goready`. So Rust→Go completions only `write()` an eventfd owned
by Go's netpoll; the actual `goready` happens inside the Go **dispatcher
goroutine** (a real M). Go→Rust crossings ride cgo/asm, where Go always has a
valid `g`/`M`.

---

# Part I — Foundation: the core fusion PoC (milestones M0–M5)

Built as reversible build-flag layers over a correct baseline; each layer had to
pass the identical stress matrix before the next was enabled.

- **M0 — Skeleton + build harness.** cargo staticlib + cgo + hand-written
  `include/sable.h` → one static binary; `sable_add` round-trips the FFI
  (`TestBuildRoundTrip`).
- **M1 — Symmetric mutual await, stock APIs only** (eventfd bridge, *two*
  epolls). Locked in the semantics, the FFI ownership model, and the
  lost-wakeup-free handshake before any linkname: tokio `AsyncFd<eventfd>` (its
  own mio) ⇄ Go `os.NewFile`/netpoll on an eventfd. 10k-concurrent stress with
  zero lost/duplicate wakeups — the semantic ground truth every later layer keeps
  green.
- **M2 — Single shared epoll** (`//go:linkname poll_runtime_poll*` + pump
  goroutines). Rebuilt tokio IO-disabled → no mio epoll. A `GoPolled` future +
  `sable_go_poll_start` + a per-fd **pump goroutine** looping
  `pollReset → pollWait → fdReady` sources all readiness from Go's netpoll.
  `TestSingleEpoll` asserts exactly one `[eventpoll]` fd. Encoded the
  **edge-triggered drain contract** (drain to `EAGAIN` before re-arming).
- **M3 — Direct park/unpark of awaiters** (`//go:linkname gopark`/`goready`).
  Replaced per-await eventfds with ONE shared completion eventfd + a dispatcher
  goroutine + a `park.go` await-slot state machine (CAS `IDLE→PARKED` in gopark's
  `unlockf` vs `Xchg→READY`+`goready` on deliver), lost-wakeup-safe.
- **M4 — Fast hot crossing** (`//go:linkname runtime.asmcgocall`). The hottest
  crossing (`sable_fd_ready`, fired per readiness event) calls `asmcgocall`
  directly, **skipping `entersyscall`/`exitsyscall`** (two P-state transitions +
  a P re-acquire). Sound only because it is short, non-blocking, non-allocating,
  and never re-enters Go. Build-tag gated with a plain-cgo fallback
  (`-tags sable_safe`).
- **M5 — Guard rails.** sysmon-retake stress, teardown, GC-vs-parked-state, and
  single-epoll-under-load tests.

### Results / findings (M-series, linux/arm64, Go 1.26.4)

- **Single epoll:** exactly one `anon_inode:[eventpoll]` fd in the process.
- **Hot crossing** (`fdReady`): asmcgocall **~7.5 ns/op** vs full cgo **~34 ns/op**
  (~4.5× — the saved `entersyscall`/`exitsyscall` + P re-acquire).
- Correct under `-race`, `GOGC=1` (aggressive GC vs parked state), and 100k+
  concurrent mutual awaits, with zero fd/goroutine/registry leaks.
- **Arch-neutral:** no hand-written per-arch asm (`asmcgocall` sets up the C ABI
  itself). Execution-verified on **arm64 (native)** and **amd64 (static-musl under
  qemu)** — full suite, single-epoll assertion, and the ~4× fast-path win on both.

### Key hazards navigated (each is a test, see source comments)

- **`goready` from a foreign thread is illegal** → route Rust→Go wakes through an
  eventfd the netpoller owns (`park.go`, `bridge.go`).
- **Edge-triggered lost wakeups:** `pollReset` erases a latched `pdReady`; a write
  before `pollOpen` yields no edge → optimistic initial readiness + read-first
  (`poll.go`, `reactor.rs`).
- **fd-reuse race:** closing a fused fd before its netpoll registration is removed
  lets another task reuse the number → the **pump is the sole fd owner and closes
  it after `pollClose`** (`poll.go`).
- **g0-stack constraints:** gopark's `unlockf` runs on g0 where sync/atomic race
  hooks and pointer write-barriers are invalid → `//go:norace`/`nosplit`/
  `nocheckptr` + `internal/runtime/atomic` + a bare-`uintptr` g pointer (`park.go`).
- **STW vs the fast path:** skipping `entersyscall` leaves the P `_Prunning`, so
  the crossing must stay short/non-blocking; verified under `GOGC=1` gctrace
  (bounded STW).

---

# Part II — Beyond the core

## goexec — running tokio tasks on Go's Ms

The dedicated executor runs tokio on *one* thread (the throughput ceiling + two
cross-thread wakeup hops). `rust/src/goexec.rs` uses `async-task` to split futures
into `Runnable`s that a pool of **Go worker goroutines (one per P)** polls, so each
task *completes on a Go M* and delivers with a **direct `goready`** — no doorbell,
no dispatcher hop.

**Decisive finding — the wakeup primitive.** A first cut parked workers in a Rust
channel's blocking `recv` (opaque to Go's scheduler) → **91 µs** conc-1 latency.
Parking each worker in Go's **netpoller** on its own per-worker eventfd doorbell
(round-robin, no thundering herd) dropped that to **4.4 µs**: Go's scheduler is
built around netpoll, so wakeups must flow through it. vs the dedicated executor:
latency 12.0 µs → **4.4 µs** (2.7×), CPU-bound 3718 → **1318 ns/op** (2.8×). Clean
under a 2M-await soak. Scope: pure-compute futures, round-robin (not
work-stealing), and it does not carry the single-epoll property.

## Inline fast path (M7)

The gap vs pure tokio was that *every* await used the async
spawn→park→cross-thread-wake→deliver path, paying a wakeup **syscall** even for
futures that never suspend. Fix (`inline.go`, `sable_future_*`): **poll inline on
the goroutine's own M**, go async only on suspension. Fast path polls once with a
no-op waker — if `Ready`, return with no spawn/gopark/syscall; only genuine
`Pending` falls back to the token-keyed waker + gopark (safe: the re-poll
re-checks readiness). **Finding:** inline compute **75 ns/op** — *beats*
pure-tokio spawn+await (~220) and is 22× under the old fusion path (1683);
fallback 4583 (1.6× < the old 7339). The common case now exceeds pure-tokio
parity, leaving ~65 ns of FFI + poll setup.

## Real-world scenario: reqwest + axum (`-tags sable_http`)

Proves the fusion holds with **full tokio-ecosystem apps**: `rust/src/http.rs`
runs **axum** + **reqwest** on a *separate* multi-thread io-enabled runtime
sharing sable's completion delivery (reqwest/axum need `tokio::net`, so this
runtime has its own mio epoll — two epolls in this scenario; the single-epoll
property is specific to the custom `GoAsyncFd` path). A Go goroutine drives a
reqwest GET and parks via `gopark`; the axum `/go` handler calls **back into Go**
(`sable_go_double`), so a request crosses Go → reqwest → axum → Go → back. 200
concurrent, zero errors, `-race`, on arm64 and amd64. The http lib references the
`sable_go_double` export, so it builds into a **separate target dir** and never
clobbers the lean core lib.

## Performance vs pure tokio (the honest cost)

`make bench-pure` runs the identical workloads in pure tokio, so the delta is
exactly the fusion boundary:

| workload | pure tokio | sable | overhead |
|---|---|---|---|
| spawn+await latency (1 in flight) | ~260 ns | ~12 µs | ~46× (two cross-thread handoffs + gopark/goready) |
| spawn+await throughput (saturated) | ~4.5M/s | ~0.83M/s | ~5.4× (~1 µs/op) |
| HTTP req/sec (`/ping`) | ~145k | ~122k | ~1.2× (~16 %) |

Takeaway: latency and trivial-compute throughput pay a real price (two
cross-thread wakeups per await), but **real I/O-bound work barely notices** — the
fusion is a poor fit for chatty nanosecond calls and a fine fit for the async I/O
that async runtimes exist for. Raw mutual-await throughput saturates by ~64
concurrent goroutines and stays flat (~800k/sec) through 32,768; a 3M-await soak
returns fds/goroutines/registries to baseline with no leaks.

---

# Part III — Productionization roadmap (R1–R6, under a self-paced `/loop`)

Ran as a loop, one tested chunk per iteration, each verified under default
`-race` + portable + `make abi-check` before being marked done. Two
roadmap-added tools later caught real bugs in *other* items (see R6b, R6c).

### R1 — CI matrix + `verify-all`
`.github/workflows/ci.yml` runs `make verify-all` (abi-check + suite + safe
variant + pipe-doorbell + http) across the certified Go × arch matrix, plus a
Go-tip tripwire job (allowed to fail) as early warning for the next release.

### R2 — Portable fallback (zero-linkname channel backend)
`make portable` / `-tags sable_portable` against the Rust core lib
(`--no-default-features`): a channel-based await backend with **zero
`//go:linkname` and zero asm** that works on any toolchain — the insurance for an
uncertified/future Go release, trading away the single-epoll pump, goexec, and
inline paths for durability.

### R3 — Generic byte-buffer `Call` API
`Call(op, req []byte) ([]byte, error)` (`call.go`) awaits an **arbitrary Rust
async op**, marshalling request/response as byte buffers (caller serializes its
own types). Explicit ownership contract: the result is a Rust-owned buffer Go
copies out (`C.GoBytes`) then frees (`sable_call_free`) — single owner, single
free. It is **core** (spawn + doorbell only), so it works under both the fast and
the portable backends. Verified under `-race` incl. 1k concurrent unique payloads.

### R4 — Cancellation
`CallCtx(ctx, op, req)`: a watcher goroutine calls `sable_call_cancel` when `ctx`
is cancelled, which aborts the tokio task (`AbortHandle::abort`), dropping its
future so `Drop` runs. **Key design:** a **publish-on-drop `CancelGuard` captured
*by* the future** guarantees exactly one completion (the real result on normal
finish, or a cancellation sentinel) even for a never-polled abort — no
double-wakeup, no orphaned result buffer, no registry leak. Verified: 500
concurrent cancels drain to 0 pending; a 10s op cancels in ~50 ms with
`context.Canceled`.

### R5 — Verification tooling (Loom · fuzz · Miri)
Added `make loom` / `make fuzz` / `make miri` and the concurrency/memory-safety
cores:
- **Loom** (`rust/tests/loom_wakeup.rs`) exhaustively model-checks the await-slot
  park/deliver protocol (`parkCommit` `CAS(IDLE→PARKED)` vs `deliverCompletion`
  `Xchg(→READY)`+`goready`): committed park ⟹ goready exactly once; aborted park ⟹
  no goready. **Finding:** the *first* model (waker + mutex + fired-flag) reported
  a "lost wakeup" — the **model** was buggy, not the protocol; rewritten to the
  await-slot pure-atomic CAS model that mirrors `park.go`, which passes.
- **Go fuzzing** (`FuzzCall`) drives arbitrary `(op, bytes)` through the `Call`
  FFI. **Finding:** it failed on byte `0xeb` — the *test oracle* used Go's UTF-8
  `bytes.ToUpper` while the Rust op does byte-wise `to_ascii_uppercase`; fixed the
  oracle, then 3.1M+ execs clean. (Library correct; test wrong.)
- **Miri** (nightly) UB-checks the `Call` pointer lifecycle; **Rust TSan** runs
  separately from Go's TSan (two TSan runtimes in one process are unstable).

### R6a — Observability
Atomic counters on `Inner` (`n_spawned`, `n_completed`, `n_cancelled`,
`peak_queue`, `in_flight`, `max_in_flight`, `n_rejected`) surfaced as
`RuntimeStats() Stats` via `sable_stats`. `InFlight` and `PeakQueueDepth` are the
producer-outpacing-consumer signals that motivated R6b. Core surface (both
backends).

### R6b — Backpressure
`SetMaxInFlight(n)` + `TryCall` returning `ErrBackpressure` immediately at
capacity (nothing spawned, nothing parked); admission is a lock-free CAS on the
`in_flight` gauge (`awaitViaParkTry` skips the park on refusal).
**Bug found & fixed (caught by R6a's `TestStats`):** the shared `publish`
decremented `in_flight` on *every* completion, but the inline/http paths publish
**without** admitting → the gauge underflowed
(`in_flight ... = 18446744073709549841`). Fixed by splitting **`publish`** (pure
transport: enqueue + peak + ring) from **`complete`** (accounting: `note_complete`
+ publish), and routing only admitted completions through `complete`.
`TestBackpressure`: 300 simultaneous callers, cap 4, gauge never peaks above 4.

### R6c — Graceful shutdown
`Shutdown()` orders teardown to close two hazards:
- **Use-after-free of `rt`**: the dispatcher dereferences the global `rt` in
  `drainCompletions`, so freeing the box while it runs is UAF. Order:
  `sable_runtime_shutdown` (abort in-flight + join executor, box stays valid) →
  close the doorbell so the dispatcher drains once more and exits → **then**
  `sable_runtime_free`.
- **eventfd leak / double-close**: `Inner` had no `Drop` (every runtime leaked its
  eventfd). Now Rust owns and closes it on drop, and Go polls a `syscall.Dup` of
  it — each side closes its own descriptor.
`TestTeardownNoLeak` (100 runtimes with tasks in flight) + `TestShutdownAndReinit`.

### R6d — Platform abstraction (the doorbell)
The one genuinely OS-specific piece, the Rust→Go completion doorbell, was
abstracted in `rust/src/doorbell.rs`: **eventfd** on Linux, **self-pipe** on
macOS/BSD (`SABLE_PIPE_DOORBELL=1` forces the self-pipe on Linux for testing —
`make test-pipe`). The doorbell carries no payload (the `(token, result)` rides
the queue), so the Go dispatcher is primitive-agnostic. netpoll linknames are
OS-agnostic at the source level.

**Roadmap infra also added:** `.github/workflows/ci.yml` gained `verification`
(loom+fuzz), `miri` (nightly, allowed-to-fail), and later the `rust2go` job.

---

# Part IV — rust2go integration

## A rust2go-style example (imitation)

*"Write an example that exhibits the use with rust2go."* Hand-wrote the rust2go
**calling pattern** over Sable's generic `Call` (zero extra deps): typed request
struct + flat codec + a typed entry point awaiting an async Rust handler (op 10),
so the call site looks like rust2go's while the async work runs on Sable's fused
runtime. Documented in `examples/rust2go.md` with the architectural contrast
(rust2go = two reactors; Sable = one shared epoll).

## Actually wiring the real rust2go crate

*"Actually wire rust2go."* Replaced the imitation with a genuine integration of
the real `rust2go` crate, behind `--features rust2go` / `-tags sable_rust2go` and
its own cargo target dir.

**Key finding — the fit is structural.** rust2go's `g2r` (Go-calls-Rust) is
**sync-only**; its codegen literally errors on an `async fn` with *"manually spawn
by your own."* That is exactly Sable's role. So: **rust2go owns the typed request
crossing** (`#[derive(R2G)]` marshalling, `#[rust2go::g2r]` C export + Go binding,
its own g0-stack `cgocall` shim); **Sable owns the async + completion** (the g2r
handler `spawn`s onto Sable's IO-disabled tokio and delivers the result via the
completion doorbell on the await token). The response rides Sable's byte-buffer
`CallResult` (g2r's sync return can't carry an async result).

**Findings / fixes:**
- The crossing panicked on Go's cgo pointer-pinning check; rust2go's own README/CI
  **require `GODEBUG=cgocheck=0,invalidptr=0`** (it passes Go pointers through ref
  structs). `//go:debug` was **rejected** ("unknown //go:debug setting" — not in
  the allowlist); GODEBUG must be an env var set at process start, so the Makefile
  `run-/test-rust2go` targets set it.
- The generated Go binding's `DemoUser` clashed with the imitation's `DemoUser`;
  resolved by renaming the Rust struct (later renamed again — see Part V demo swap).
- The generated binding is **committed**, so build/test needs only the crate + Go
  module (no `rust2go-cli`); `make gen-rust2go` regenerates and re-prepends the
  `//go:build sable_rust2go` tag the cli omits.

---

# Part V — Publish-ready (recent session)

## Restructure into an importable library

*"To make this PoC publish-ready, separate example/benchmarking code from the
library core."* Turned a single `package main` in `go/` into an importable Go
library with examples/benchmarks split out.

| Before | After |
|---|---|
| `go/*.go` (`package main`) | **repo root = `package sable`**, `github.com/moriyoshi/sable` |
| `go/main.go` | `cmd/sable-demo/` (builds `./sable`) |
| `go/rust2go.go` | `examples/rust2go/` (pure Go over `sable.Call`) |
| `go/r2g*.go` | `examples/rust2go-real/` (real crate, `-tags sable_rust2go`) |
| `go/http*.go` | `examples/http/` (reqwest+axum, `-tags sable_http`) |
| `go/bench_*_test.go` | `bench/` (public-API microbenchmarks) |
| demo op handlers in `rust/src/lib.rs` | `rust/src/demo.rs` (module split) |

**Public API added** (`api.go`/`api_fast.go`/`api_asm.go`/`call.go`): `Init`
(idempotent `sync.Once`; every entry point calls it lazily), `AwaitRust`,
`AwaitToken(spawn)→uint64`, `AwaitCallResult(spawn)→[]byte`, `RuntimePtr()→uintptr`,
`CountEpollFds`, `RustAwaitsGo`, `ReadPipeViaRust`, `CrossingCgo`, `CrossingAsm`.

**Key finding — why `RuntimePtr` + `AwaitToken`/`AwaitCallResult` exist.** cgo
types are **package-scoped**: an example package cannot see the library's
`C.SableRuntime`, so it can't call the shared runtime's cgo functions directly.
The advanced examples (http, rust2go-real) take the runtime as a `uintptr`
(`RuntimePtr`) and cast it in their own cgo, and park/deliver through the public
`AwaitToken` (raw u64) / `AwaitCallResult` (byte-buffer) primitives. This is the
crux that made a clean library boundary possible.

**Other decisions / findings:**
- Demo ops moved to `rust/src/demo.rs` as a **module split, not a feature gate** —
  a `demo` feature would force `--features demo` onto every build variant and risk
  one missing it.
- Public-API benchmarks → external `bench/`; benchmarks over unexported internals
  (inline/goexec/stress) stay in-package (`_test.go` can't `import "C"`, and you
  can't bench unexported symbols externally).
- Opt-in examples each have a tagged real file + a `!tag` `stub.go` so plain
  `go build ./...` compiles without the special libs.
- `TestMain`s route through `Init()` (the `Once`) so the lazy `Init()` never
  double-builds; `shutdown_test.go` keeps raw `sableInit()` for its
  shutdown→reinit cycle.

**Bug found & fixed:** `make test-pipe` returned `(cached)` —
`SABLE_PIPE_DOORBELL` is read by Rust at process start and does **not** key Go's
test cache, so it reused the eventfd result and never exercised the self-pipe.
Added `-count=1` (pre-existing latent bug).

**Also:** `.gitignore` (binaries + `rust/target*`); Makefile/CI paths →
`./...`, `.`, `./cmd/sable-demo`, `./examples/*`; README Layout + "use as a
library" sections.

## Consolidated the three docs into README.md

Deleted `PLATFORMS.md`, `SUPPORTED.md`, `VERIFICATION.md`; folded their full
content into self-contained README sections — **Verification**,
**Supported Go toolchains**, **Operating-system support**. Repointed the four
references in files that stay (`shutdown_test.go`, `rust/src/doorbell.rs`, two CI
comments). **Gotcha:** a `sed` repoint injected nested double-quotes into a Go
string literal in `shutdown_test.go`, breaking the build; fixed by dropping the
inner quotes.

## Replaced the age-verification demo

*"Age verification example is inappropriate. Replace it with something."* The
`check_user` / `DemoUser{name, age}` / `age >= 18` / "welcome"/"must be 18" demo
became an **inventory stock check**: `Order{item, qty}` → `qty <= 20` (20 on hand)
→ "`item` reserved" / "only 20 `item` in stock". Same typed-request →
async-lookup → typed-reply shape and the same straddles-a-boundary concurrency
test. Touched `rust/src/demo.rs` (`check_stock`), `rust/src/r2g.rs`
(`Order{item, qty}`), the **regenerated** `examples/rust2go-real/r2g_gen.go` (via
`make gen-rust2go`, so the rename cascaded instead of being hand-edited), both
examples' Go + tests, `examples/rust2go.md`, and the README. `make run-rust2go`
now prints `CheckStockReal(widgets) -> ok=true message=widgets reserved` and
`(gizmos) -> ok=false message=only 20 gizmos in stock`.

## Rendered the README ASCII diagrams in Mermaid

The boxes-and-arrows **architecture** figure and the **Layout** brace grouping
became GitHub-native ```mermaid``` `flowchart` blocks (Layout keeps a plain-text
file index below its structure diagram). No box-drawing art remains in README.

## This journal

Written, then expanded to cover the whole project — from the M0–M5 foundation and
the goexec/inline/http explorations through the R1–R6 roadmap, rust2go, and the
publish-ready work.

## The embedding surface (S-1..S-5) — zero-copy binding support

`../imbh-go/docs/prescription-sable.md` asked sable to become **hostable** by an
out-of-tree crate (a zero-copy Arrow / DataFusion binding) instead of only
running its own demo ops. Five items, all **additive** — no change to the
doorbell, the park/deliver state machine, the dispatcher, or the single-epoll
invariant — now merged to `main` (commits `cda38a3` S-1/S-2/S-5, `433869b`
S-4.2, `44c067b` S-3, `f08bd91` docs; branch `zero-copy-binding-phase1` deleted).

- **S-1 — pluggable handler registry** (`rust/src/registry.rs`).
  `sable::register(op, handler)` lets a depending crate host `op -> async handler`
  before `Init`, so the byte-buffer Call path is no longer hardwired to the demo
  ops. A shared `dispatch()` backs `sable_call`/`_try`/`_ctx`: **registry first,
  demo fallback second**. A blanket `AsyncHandler` impl makes the common case a
  plain closure; the table is a `LazyLock<Mutex<HashMap>>` read as a cloned `Arc`
  per Call so **the lock is never held across `.await`**.

- **S-2 — opaque handle (zero copy).** A handler may return `Payload::Handle{ptr,
  release}` instead of `Payload::Bytes`. sable delivers `ptr` as the ordinary
  `(token, u64)` completion — **it never inspects the pointer** — and Go takes
  ownership via `sable_call_handle` / `CallHandle`. The safety net mirrors
  `CancelGuard`: a `token -> release` map is armed *before* delivery and swept on
  shutdown, so a handle Go never takes (shutdown/abort/never-taken) gets
  `release(ptr)` driven **exactly once**. `sable_call_handle_taken` disarms it on
  the happy path. `0` is the reserved "no handle" sentinel on the wire
  (`ErrNoHandle`).

- **S-3 — streaming cursor.** A stream op yields many batches lazily without
  materializing the whole result and **without `block_on`**. The model:
  "async get_next" = a **bounded mpsc**. `register_stream(op, |req, tx| async…)`
  is a *producer* that pushes each batch (a `Payload`) into `tx`; returning ends
  the stream. `sable_stream_open` runs the producer + registers a cursor;
  `sable_stream_next` awaits **one** batch and completes the token with its handle
  (`0` = end-of-stream) — each Next is a fresh single completion, the goroutine
  parks, **no M blocked**; `sable_stream_close` aborts the producer (dropping the
  pinned stream, e.g. releasing a DataFusion snapshot) and drains buffers.
  Per-batch handles reuse the S-2 release net. The **bounded channel IS the
  backpressure** — the producer awaits capacity, and a dropped receiver makes the
  next `send` fail so the producer stops cleanly. Go glue: `Stream` / `OpenStream`
  / `Next` / `Close`. Cursors are also swept on shutdown (`drain_cursors`).

- **S-4.2 — multi-thread executor keeps the single epoll.** For CPU-heavy
  handlers (DataFusion) that need real parallelism, an opt-in `multithread`
  feature adds `new_multithread` = an **IO-disabled** `new_multi_thread()
  .enable_time()` executor. **Empirically validated** (`make test-multithread`,
  `lib.rs s4_epoll_tests`): multi-thread + `enable_time` (no `enable_io`) → **0
  epoll**; `enable_all()` + a bound socket → **≥1** (control). This proves the
  epoll is **IO-driver-sourced, not scheduler-sourced** — the same trick that made
  the current-thread runtime own zero epoll holds for the multi-thread one. `new()`
  and `new_multithread()` share wiring via `from_tokio()`; the symbol is absent
  from the default `libsable.a`.

- **S-5 — the embedder owns the lib.** `crate-type += rlib`; a `demo` feature
  (default-on) gates every demo op; cgo LDFLAGS split into `link_default.go` /
  `link_extern.go` so `-tags sable_extern_lib` lets the binding supply a combined
  staticlib via `CGO_LDFLAGS`. Verified by linking `cmd/sable-demo` against a
  **no-demo core**.

**Key finding — one completion primitive carries everything.** Bytes, opaque
handles, and per-batch stream items all ride the *same* `(token, u64)` doorbell
completion the original design shipped; S-2/S-3 add no new transport, only new
*interpretations* of the `u64` (a result length vs. a raw pointer vs. a cursor
id / end sentinel). The zero-copy path is "free" precisely because the doorbell
never dereferences what it delivers.

**Key finding — release-on-drop generalizes.** The exactly-once cleanup net first
built for `CancelGuard` (R4) turned out to be the right shape for *any* resource
handed across the boundary: opaque handles (S-2) and open cursors (S-3) both
arm a release/abort keyed by token, swept on the same shutdown path. New
ownership, zero new lifecycle machinery.

**Scope boundary.** The remaining pieces belong to imbh-go, not sable: the actual
Arrow FFI export inside handlers, S-4.1 `spawn_blocking` wrapping (binding-side),
and wiring Go's `Init` to select the multi-thread runtime. sable ships the
*surface*; the binding brings the engine. This also partly retires the standing
follow-up "a user-populated dispatch registry replacing the demo ops" — the
registry now exists (S-1); the demo ops remain only as the fallback.

**Docs.** `f08bd91` folded all of this into the README — a new **Embedding**
section, an **Ownership & lifetimes across the boundary** section (per-transport
object/blob ownership across the FFI), and the `demo`/`multithread`/
`sable_extern_lib` build knobs. Tests: registry bytes/handle + byte-path
misuse-release (Rust); `CallHandle` end-to-end zero-copy read+release, stream
full-drain / early-close-cancel / unknown-op (Go, `-race`) — green under both the
fast and portable backends.

## Post-implementation gap review of the embedding surface (A–F)

With S-1..S-5 shipped, re-read the imbh-go prescription against the *actual*
implementation and asked what new gaps surfaced. The read/happy paths were solid;
every gap was on the **control/robustness axis**. First a foundational check the
prototype had never actually run: does a *combined* crate (a second crate
depending on `sable` as an `rlib`) export sable's `#[no_mangle]` C symbols into
its staticlib, and is the global handler registry shared across that crate
boundary? Built a throwaway `combined` crate + a Go program under
`-tags sable_extern_lib`: `sable_*` symbols survive, and a handler registered by
the combined crate is reached by `sable_call` from Go. **S-5 verified end-to-end**,
not just "symbols present."

Six gaps found and closed in `de8ac41`:

- **A — no cancellation on the handle/stream paths.** The byte path had
  `sable_call_ctx`; the handle path had none, and streams had only `Close`
  ("must not race an in-flight `sable_stream_next`"). Added `sable_call_handle_ctx`
  / `CallHandleCtx` and `sable_stream_next_ctx` / `Stream.NextCtx`, both reusing
  the `CancelGuard` discipline (the stream one publishes the end sentinel instead
  of a Call completion). A `Next` parked on a slow batch can now be cancelled from
  another goroutine — the DataFusion mid-scan cancel the prescription's S-3 wanted.
- **B — the multi-thread executor was unreachable from Go.**
  `sable_runtime_new_multithread` existed but `sableInit` hardcoded the
  single-thread ctor. Added a build-tagged `newRuntime()` seam
  (`-tags sable_multithread`, `SABLE_EXECUTOR_THREADS`). `make test-multithread`
  now also runs an end-to-end Go test asserting the fused program keeps **one
  epoll** on the multi-thread executor.
- **C — streams bypassed backpressure.** `sable_stream_open`/`_next` used
  `note_spawn`, so `SetMaxInFlight` had zero effect. Made `sable_stream_open`
  admission-controlled (`try_admit`) and, crucially, **hold the in-flight slot for
  the cursor's lifetime** (released in `close`/`drain`), so the cap bounds
  concurrent *live* streams; `OpenStream` returns `ErrBackpressure`. Batch `next`s
  publish without touching the gauge (continuations of an admitted cursor).
- **D — handle-path errors were lossy.** The prototype delivered `0` and expected
  the caller to re-run via the byte `Call` — unsafe for non-idempotent queries.
  Added `sable_call_handle_error(token)`: the handler's error is boxed as a
  `CallResult` keyed by token and read once, no re-execution.
- **E — an exported-but-unsent batch could leak.** `Payload::Handle` had no `Drop`,
  so a batch a producer exported then dropped (aborted mid-stream, buffered at
  close, misrouted to the byte path) leaked its pointer. Reshaped `Payload::Handle`
  around a **self-releasing `Handle`** (frees on drop unless disarmed via
  `into_raw`). Delivery disarms and transfers to Go; every other drop frees once.
  Collapsed the drain loops to plain channel drains. (Aliased `tokio::runtime::Handle`
  → `TokioHandle` to free the name.)
- **F — registration had no C/Go entry or ordering guard.** Kept registration
  Rust-only (driven from the combined crate before `Init`) but documented the
  contract and confirmed an unregistered op returns a clean `"unknown op N"` error
  on every path — not UB. The combined-crate e2e above exercises exactly this.

Tests added: handle ctx-cancel, stream next-cancel (against a *stalling* demo
stream so the pull genuinely parks), stream backpressure, unknown-op error
fidelity (Go, `-race`); multi-thread single-epoll e2e. README **Embedding**,
**Limitations**, and **Ownership & lifetimes** sections updated to match. Full
Go `-race`, Rust, portable, and multithread suites green; the combined-crate S-5
e2e re-verified against the changed `Payload` API.

---

# Part VI — Review follow-ups

## Empty-result pointer on the byte Call ABI (GC sentinel hazard)

Reviewing a patch to `sable_call_result`, confirmed a real latent crash on the
**empty-result** path. An empty Rust result buffer (`Vec::new()`, e.g. `OpEcho`
with an empty request) makes `Vec::as_ptr` return the **dangling-but-aligned
sentinel** `NonNull::dangling()` — `0x1` for `u8`. The byte path handed that raw
value back through the `out_ptr` FFI slot, where the Go caller (`callResultBytes`)
stored it in a `*C.uint8_t` — a **pointer-typed** stack slot, live across the
following `C.GoBytes`/`C.sable_call_free` calls.

Go's runtime (`runtime/stack.go`, `adjustpointers`) throws
`invalid pointer found on stack` for any live pointer slot holding
`0 < p < minLegalPointer` (4096) when a goroutine's stack is **copied** (a
`morestack` growth at the `GoBytes` call site), with `debug.invalidptr` on by
default. `0x1` fits exactly. Intermittent — needs a stack copy while the sentinel
is live — but a genuine crash, and it violates the cgo rule against parking a
non-Go address in Go pointer-typed memory regardless.

Fixed on both sides (belt-and-suspenders; either alone suffices):
- **rust** — `sable_call_result` returns a **NULL** `out_ptr` for an empty buffer
  (`core::ptr::null()`) instead of the dangling sentinel.
- **go** — `callResultBytes` holds the address as a **`uintptr`**, never a
  `*C.uint8_t`, so the GC never scans it as a heap pointer; it materializes an
  `unsafe.Pointer` only transiently for the copy and only when `n > 0 && ptr != 0`.
  (A cosmetic change: an empty successful result now yields a `nil` slice rather
  than a zero-length one — harmless; callers and `string()` treat them alike.)

Tests added:
- **rust** (`call_unsafe_tests`) — `empty_result_returns_null_ptr` and
  `empty_err_result_returns_null_ptr` pre-seed the out-pointer with the `0x1`
  sentinel and assert `sable_call_result` nulls it on the ok **and** err branches;
  `nonempty_result_returns_valid_ptr` guards against over-nulling the live path.
  (Verified the assertions bite: reverting the Rust fix fails
  `empty_result_returns_null_ptr`.) The pre-existing
  `empty_result_has_null_or_valid_ptr` only checked `len == 0` and passed either
  way — replaced.
- **go** (`call_empty_test.go`) — `TestCallEmptyResult` for empty `nil`/`[]byte{}`
  requests, plus `TestCallEmptyResultGCStress`: 32 goroutines hammer the
  empty-result path under aggressive GC (`SetGCPercent(5)`) entering the FFI from a
  freshly-grown stack (`deepCallEcho` recurses), a probabilistic regression guard
  for the sentinel hazard. Rust `call_unsafe_tests` and Go `-race` suites green.

## Go-tip tripwire: `internal/runtime/atomic` lost its linkname pushes

The `go-tip` CI job (the allowed-to-fail early-warning tripwire from R1) started
failing to **link** the canary binary on Go tip:

```
link: github.com/moriyoshi/sable: invalid reference to internal/runtime/atomic.Load
```

and, once `Load` was addressed, the same wall one symbol further on:

```
link: github.com/moriyoshi/sable: invalid reference to internal/runtime/atomic.Cas
```

**Root cause — a linker *policy* change, not an ABI/behavior break.** Go tip
deleted the `//go:linkname` **push** directives from `internal/runtime/atomic`
(the *"Export some functions via linkname to assembly in sync/atomic"* block that
pushed `Load`/`Loadp`/`Load64` through Go 1.26; only `storePointer` retains a push
on tip). Under the default `-checklinkname=1`, `cmd/link`'s `checkLinkname`
(`cmd/link/internal/loader/loader.go`) permits an external package to *pull* a
std **assembly** symbol only if that symbol's ABI wrapper carries a push linkname
(`osym.IsLinkname()` / `IsLinknameStd()`). With every push gone, **all** of
sable's `park.go` await-slot atomics — `Cas`/`Xchg`/`Store64`/`Load`/`Load64` —
become unlinkable from an out-of-tree package. The linker `log.Fatalf`s on the
**first** bad reference, which is why the failure surfaces one symbol at a time
and why "fix `Load`, reveal `Cas`" looked like whack-a-mole. The atomic
primitives themselves are unchanged, so the canaries *pass* the moment they link.

**Fix — relax the check on the tripwire only (`ci.yml`, commit on branch
`fix/go-tip-atomic-load-linkname`).** The `go-tip` step now runs
`gotip test -ldflags=-checklinkname=0 …`. Rationale: this job is
`continue-on-error: true` and its real purpose is to certify runtime **behavior**
via `TestCanary*`; `-checklinkname=0` lets the canaries link and actually run on
tip. The `Makefile` already documents `-checklinkname=0` as the sanctioned escape
hatch, and the certified `verify` job (Go 1.26.4) keeps the default
`-checklinkname=1` as the real gate for shipped code, so no shipping regression is
masked. Result: all four canaries — `TestCanaryAsmcgocall`,
`TestCanaryInternalAtomics`, `TestCanaryGoparkGoready`, `TestCanaryNetpoll` —
**PASS** on tip.

**Alternatives weighed and rejected (for now):**
- *Switch the loads to `sync/atomic`.* An earlier attempt (orphaned commit
  `8c6e4ab`) replaced `atomicLoad`/`atomicLoad64` with `sync/atomic` since their
  call sites all run on a normal g after `gopark` returns. Insufficient: it only
  rescues `Load`/`Load64`; `Cas`/`Xchg`/`Store64` still break, and the `Cas` in
  `parkCommit` runs on **g0** where it must stay `nosplit` and race-hook-free —
  `sync/atomic`'s race instrumentation is invalid there under `-race`. So the
  linkname dependency cannot be fully dropped by swapping in `sync/atomic`.
- *Provide our own amd64/arm64 assembly stubs* for `Cas`/`Xchg`/`Store64` (our
  symbols, `nosplit`, no race hooks, immune to `checklinkname` on any future Go).
  This is the **durable** fix, deferred here because it reintroduces
  arch-specific asm — regressing `arch.go`'s "no hand-written per-arch asm, any
  64-bit arch just works" property — which is a heavier change than an
  allowed-to-fail tripwire warrants right now.

**Forward warning (the tripwire doing its job).** When this `internal/runtime/
atomic` change reaches a **stable** Go, sable's *default* build (`-checklinkname=1`)
breaks for every user, not just CI. The asm-stub fix should land before then; it
is tracked in the open follow-ups below.

---

## macOS: the fast path did not actually compile (eventfd outside the doorbell)

- **Symptom.** A downstream embedder (imbh-go) cross-building the combined staticlib
  for `aarch64-apple-darwin` / `x86_64-apple-darwin` in CI failed with 6 × `E0425`:
  `cannot find function eventfd` / `EFD_NONBLOCK` / `EFD_CLOEXEC` in `libc`. No Go
  step involved — the Rust lib itself does not build for Apple targets.
- **Why the README was wrong.** It claimed the completion doorbell was "the one
  genuinely OS-specific piece" and that macOS needed "no further Rust/Go changes
  beyond certification". `doorbell.rs` *is* properly abstracted, but two other sites
  used `libc::eventfd` unconditionally: `goexec.rs` (per-worker doorbells, via its own
  duplicate `make_eventfd`) and `lib.rs` (the `rust_awaits_go` helpers). The
  `SABLE_PIPE_DOORBELL=1` test mode only exercised the *completion* doorbell, so this
  never showed up on Linux.
- **Fix 1 — `goexec.rs` onto `Doorbell`.** `Worker.efd: RawFd` → `Worker.bell:
  Doorbell`, `make_eventfd()` deleted, the raw 8-byte `libc::write` in `schedule()` →
  `bell.ring()` (which already sizes the write per primitive), and
  `sable_goexec_worker_efd` returns `bell.read_fd()`. **No Go change needed** —
  `goexecWorker` already reads up to 8 bytes then drains the queue, exactly the
  contract `doorbell.rs` documents. Bonus: `make test-pipe` now covers goexec too, so
  the self-pipe mode exercises both doorbell users.
- **Fix 2 — `rust_awaits_go` is Linux-only, deliberately.** Unlike a doorbell, its
  eventfd is a *value channel* (Go writes a `u64` Rust reads) registered with Go's
  netpoller under a **single** fd; `sable_go_compute(kind, arg, efd)` hands Go the same
  fd Rust awaits. A pipe/socketpair has distinct ends, so porting means widening that C
  ABI to two fds and reworking an fd-ownership close protocol the code itself flags as
  fd-reuse-race-sensitive. It is demo/benchmark surface used by no embedder, so it is
  gated to Linux. The two **entry points stay present on every target**
  (`sable_spawn_rust_awaits_go`, `sable_future_new` kind 5) because `bridge_fast.go`
  calls them unconditionally — gating the symbols out would break the Go link off
  Linux. Off Linux they fall back to a plain compute and still complete their token, so
  no goroutine hangs.
- **Verification.** `cargo check --target {aarch64,x86_64}-apple-darwin` is clean with
  zero warnings — and needs **no macOS SDK**, since `check` never links, so this is
  enforceable from a Linux box and worth wiring into CI. Linux is unchanged (every new
  `cfg` is `target_os = "linux"`): `make test`, `make test-safe`, `make test-pipe` all
  green under `-race`, and `TestGoExec` passes 3/3 under `SABLE_PIPE_DOORBELL=1`,
  which is the macOS primitive. Pre-existing and untouched: 32 `not_unsafe_ptr_arg_deref`
  clippy errors (identical count at the parent commit) and `cargo fmt` drift in
  `pure_bench.rs` / `registry.rs` / unrelated `lib.rs` sites.
- **Still open.** Real-hardware certification (`GOOS=darwin make abi-check`); the
  `/proc/self/{fd,status}` assertions in the fast-only stress/robust suites; and the
  two-fd port of the value channel if an embedder ever needs it.

## macOS: making `make test` actually run on Apple hardware (Go side + certification)

The prior entry got the *Rust* lib to `cargo check` clean for Apple targets, but off
a real `darwin/arm64` box `make test` still failed at four layers below Rust — three
build/link blockers, then the runtime assertions. Fixed all four; two of that entry's
three "Still open" items are now closed.

- **Blocker 1 — `syscall.Pipe2` is Linux-only.** `bridge_fast.go` and `canary_test.go`
  built the fused pipe with `syscall.Pipe2(fds, O_NONBLOCK|O_CLOEXEC)`, which does not
  exist on darwin (`undefined: syscall.Pipe2`, hard build failure). Added
  `pipe_util.go` (`!sable_portable`) with a portable `nonblockingPipe` that does
  `syscall.Pipe` + `CloseOnExec` + `SetNonblock` under `syscall.ForkLock.RLock` (the
  fork-race window `Pipe2`'s atomic flags close on Linux; the RLock is the standard-lib
  idiom for the two-syscall fallback). Both call sites now go through it.
- **Blocker 2 — the cgo link line hardcoded Linux system libs.** `link_default.go` /
  `link_extern.go` carried `-lgcc_s -lutil -lrt -lpthread -lm -ldl` in a single
  `#cgo LDFLAGS`; darwin has none of `gcc_s`/`rt`/`util`/`dl` (`ld: library 'gcc_s'
  not found`). Split into `#cgo linux LDFLAGS:` (unchanged) and `#cgo darwin LDFLAGS:`.
  `cargo rustc --release -- --print native-static-libs` reports the darwin staticlib
  needs `-liconv -lSystem -lc -lm`; libSystem already provides libc/libm and cgo links
  it by default, so `-liconv` is the **only** genuinely-extra flag (keeping `-lSystem`
  drew a `duplicate libraries` warning). `-lsable`/`-L` stay in the shared directive so
  ordering (lsable before its deps) is preserved.
- **Blocker 3 — the ABI guard fails closed on an uncertified toolchain.** This box runs
  **go1.26.5**; `SupportedGoVersions` listed only `go1.26.4`, so `requireVerifiedGo`
  panicked in the constructor. Ran the documented certification (`make abi-check`): all
  four behavioral canaries (gopark/goready, asmcgocall, netpoll, internal atomics) PASS
  and all five linknamed symbols are present in the linked binary on `darwin/arm64` +
  go1.26.5 — so the internal ABI is intact here, not just assumed. Added `go1.26.5` to
  the list. (`go.mod` still pins `toolchain go1.26.4`; per the standing fact this is an
  ABI re-audit each bump — done for .5.)
- **Runtime failures — four Linux-only assertions, gated not ported.** Once it built,
  four tests failed because they assert Linux-only behavior, consistent with the prior
  entry's design decision:
  - `TestRustAwaitsGo` / `TestSingleEpoll` (the `demoRustAwaitsGo == 7` leg) — off Linux
    the value channel falls through to `run_demo`, so kind 0 returns **42** (the
    Rust-side sleep result), not 7 (the *Go* compute). The Go-compute-over-netpoll path
    is exactly what stays Linux-only.
  - `TestInline`'s kind-5 leg — `sable_future_new` kind 5 falls through to `run_demo`,
    which has no kind 5 (`_ => 0`), so it returns **0**, not the echoed arg. The kind-2/4
    inline fast paths still run and are asserted on every OS.
  - `TestSingleEpoll` / `TestSingleEpollUnderLoad` (the epoll count) — `countEpollFds`
    reads `/proc/self/fd` for `[eventpoll]` and returns **-1** off Linux; the invariant
    is meaningless without epoll (darwin uses kqueue).
  Added a `skipNonLinux(t, what)` helper (in `sable_test.go`, shared across the
  `!sable_portable` in-package tests) and gated exactly those legs. This matches the
  *existing* graceful-degradation pattern: `TestNoResourceLeak` / `TestRuntimeTeardown`
  already no-op their `/proc` checks via `-1` sentinels — these four just lacked the
  guard. `readPipeViaRust` (reactor-based `go_read_pipe`, no `cfg`) and the kind-1 echo
  stress **do** run on darwin and pass, so the fused-fd reactor path has real coverage.
- **Verification.** On `darwin/arm64` + go1.26.5: `make test`, `test-safe`, `test-pipe`,
  `test-portable` all green under `-race`; `make abi-check` PASS.
- **Still open (unchanged).** The two-fd port of the `rust_awaits_go` value channel if
  an embedder ever needs Go-compute-over-netpoll on non-Linux — the one remaining item
  from the prior entry. macOS build + `-race` test + abi-check are now covered on real
  hardware.

---

## CI: Darwin + Windows platform jobs (`.github/workflows/ci.yml`)

The two macOS entries above got `make verify-all` green on a local `darwin/arm64`
box; this makes the platform coverage part of CI rather than a one-off local run.
Two jobs added; the interesting work was mostly in Windows and in CI hygiene, not
in the macOS job itself.

- **macOS — `verify-macos`.** Mirrors the Linux `verify` gate exactly: `make
  verify-all` (abi-check + fast `-race` + `sable_safe` + `test-pipe` + `sable_http`)
  under `dtolnay/rust-toolchain@stable` + `setup-go 1.26.4`. Green in CI on `macos-14`
  in ~1 min (the http leg compiles reqwest/axum from scratch on a cold runner). This
  is the same full suite, now on the self-pipe doorbell + kqueue netpoll instead of
  eventfd + epoll — the exact primitives the macOS port added.
- **Intel (`macos-13`) dropped.** First cut matrixed `[macos-14, macos-13]` for
  arm64 + amd64 parity with the Linux side. But GitHub's Intel macOS runners are
  scarce: the `macos-13` leg sat in `queued` for **20+ minutes with no assignment**,
  stalling the whole run (the arm64 leg had long since passed). Dropped it to
  `macos-14` only — the macOS-specific seams (kqueue, self-pipe) are arch-independent,
  and the amd64 *ABI* is already certified by the Linux `verify` matrix, so a
  macOS/amd64 leg bought coverage we already have at the cost of CI reliability.
- **Windows genuinely does not build — and that shaped the job design.** Unlike
  macOS, Windows is not a certification gap but a *source* gap: `rust/src/doorbell.rs`
  opens with `use std::os::unix::io::RawFd;` (and the whole self-pipe model is fd-based),
  so even the portable staticlib (`--no-default-features --features demo`) fails to
  compile — the IOCP-associated doorbell handle is unimplemented (a documented
  follow-on, README "Operating-system support"). There is no green Windows path today.
- **CI-hygiene finding — a job-level `continue-on-error: true` job still shows a red
  ✗ check on the PR.** The first Windows cut was an "allowed to fail" build job, by
  analogy with the `go-tip`/`miri`/`rust2go` tripwires. But those are green *because
  their commands currently pass*; a job that always fails leaves a red check on the PR
  even with `continue-on-error` (it only stops the failure from failing the *run* — the
  per-job check still reports failure). So the PR looked like it was failing CI.
- **Fix — an expected-failure (xfail) tripwire, the inverse of `go-tip`.** Instead of
  "allowed to fail", the Windows job *asserts the known gap*: it runs `cargo build` and
  - if the build **fails** and the stderr matches `os::unix|RawFd` → **green**, emits a
    `::notice::` that the gap still stands (this is the normal, expected state);
  - if the build **succeeds** → **red** (`::error::` "promote this tripwire to a real
    build + `go test -tags sable_portable` job, and add a `#cgo windows LDFLAGS` line"),
    because Windows support has landed and CI should now actually test it;
  - if the build fails for **any other reason** → **red**, a new/unexpected breakage to
    investigate (guards against the job going green on an unrelated toolchain failure).
  Green today (~45 s), and it flips to an actionable red exactly when the state changes.
  `go-tip` is green while tip works and red when it breaks; this is the mirror — green
  while the platform gap holds, red when it closes.
- **No new link-flags change.** The macOS `#cgo darwin LDFLAGS: -liconv` split from the
  prior entry already covers the cgo link off Linux; this change is CI + docs only. When
  Windows support lands it will additionally need a `#cgo windows LDFLAGS` line for the
  staticlib's system libs (noted in the tripwire's `::error::` message).
- **Verification.** `verify-macos` green on `macos-14`; `windows-portable-tripwire`
  green (asserting the gap); all Linux jobs unchanged and green. `make verify-all`
  re-confirmed exit 0 on local `darwin/arm64` (go1.26.5).

---

## Emulation harnesses: amd64 under qemu-user + Windows under Wine

Two emulation harnesses so amd64 and Windows can be exercised from an arm64 dev box
(and Windows in CI without a Windows runner). Closes the `windows-portable-tripwire`
gap from the previous entry — the Windows portable fallback now **builds and runs**.

- **The qemu-user gotcha that shaped both harnesses: rustc/LLVM segfault under
  qemu-user.** The first cut ran the whole build *inside* an emulated amd64 container
  (`docker run --platform linux/amd64` + build). `cargo` died at `rustc -vV` with
  `SIGSEGV` under qemu-user — reproducible even after pinning `tonistiigi/binfmt` to
  `qemu-v9.2.2` (a known qemu-user limitation with LLVM's threading/mmap). So the model
  had to flip: **cross-compile natively, emulate only the test binaries.** A native-arch
  toolchain image cross-builds with zig as the cgo cross C compiler; binfmt then runs
  just the resulting amd64 test binaries under qemu. Spike-validated: a dynamic amd64
  cgo binary runs under qemu in a native arm64 container once `libc6:amd64` is present.
- **amd64 harness (`make test-qemu-amd64`, LOCAL only).** `ci/Dockerfile.emu` (native
  Go+Rust+zig) + `ci/emu-suite.sh`. Uses the **gnu** triple, not musl: musl-static would
  need the shipped `-lgcc_s -lrt -lutil -ldl` cgo flags dropped, whereas gnu keeps
  `link_default.go` untouched and `libc6:amd64` gives qemu the runtime loader. Both the
  portable AND the fast (asmcgocall + linkname) suites pass under qemu locally. No CI job
  — CI already covers amd64 natively; the harness's value is the arm64 dev box. binfmt is
  installed via a **digest-pinned** `tonistiigi/binfmt` (a bare `multiarch/qemu-user-static
  --reset` is amd64-only, so it can't self-register from an arm64 host — and `--reset`
  destructively clobbered Docker Desktop's existing handlers mid-debug).
- **Windows portable port — only `doorbell.rs` blocked it.** Every `RawFd` use in
  `lib.rs` is already `#[cfg(feature = "fast")]`-gated and `reactor.rs`/`goexec.rs` are
  fast-only mods, so in the portable build the *sole* `std::os::unix` dependency was the
  doorbell. Split it `#[cfg(unix)]` (unchanged eventfd/self-pipe) vs `#[cfg(windows)]`: an
  anonymous **pipe** (`CreatePipe`), write end set `PIPE_NOWAIT` so `ring()` never blocks
  the executor when the buffer fills (Go drains the whole logical queue on any wake), read
  end left blocking. The read HANDLE crosses the existing `sable_completion_efd -> c_int`
  FFI narrowed to 32 bits (Win32 handles are 32-bit-significant for interop). Go side:
  `bridge_dup_{unix,windows}.go` split `syscall.Dup` vs `DuplicateHandle`; the dispatcher
  **blocking-reads** the pipe HANDLE — fine, because the zero-linkname portable build has
  no netpoller access anyway, and Go's `os.NewFile` on a non-overlapped pipe falls to
  blocking-on-a-thread. `#cgo windows LDFLAGS` from `rustc --print native-static-libs`:
  `-lkernel32 -lntdll -luserenv -lws2_32 -ldbghelp`.
- **Windows CI is NATIVE (`verify-windows` on `windows-latest`); Wine is the local aid.**
  First cut ran the suite under Wine on an ubuntu runner; switched to a real `windows-latest`
  runner — the authentic test (real Windows kernel + pipe semantics), and simpler (no
  cross-compile, no Wine). It builds the staticlib for `x86_64-pc-windows-gnu` (to match the
  runner's mingw gcc that cgo uses) and `go test -tags sable_portable` natively. For
  non-Windows dev boxes, `make test-wine` (`ci/wine-suite.sh`) cross-compiles a static cgo
  test binary via mingw-w64 and runs it under Wine (native on an amd64 host; via Rosetta on
  Apple Silicon). Replaces the allowed-to-fail tripwire with real coverage.
- **Local validation reached, and its limits.** Validated locally in Docker: the Rust
  windows-gnu portable lib compiles; the Go windows/amd64 test binary cross-compiles and
  statically links (caught one real bug — `syscall.GetCurrentProcess` returns two values on
  Windows); the amd64 qemu suites pass. The Windows **runtime** (doorbell behavior) is
  validated by the native `verify-windows` CI job — Wine-under-qemu on this arm64 box stalls,
  so it was not validatable locally. (A first CI attempt also caught that ubuntu's `wine64`
  package ships only `libwine`, no launcher — moot once CI went native.)
- **Still open (Windows) → now partly closed.** The FAST path on Windows split into a
  tractable half (the crossing/await machinery, landed in the next entry) and a design
  project (fd fusion on IOCP). See below.

---

## Fast crossing on Windows (fast path minus fd fusion)

The fast build is really two separable things, and only one of them is Unix-bound. Split
them at a build-tag seam and the OS-neutral half compiles, links, and runs on
`windows/amd64`; the Unix-bound half stays behind a `unix` constraint.

- **The seam is readiness-vs-completion, not "fast-vs-portable".** The crossing/await
  machinery — the `asmcgocall` fast crossing (`trampoline.go`), `gopark`/`goready` await
  (`park.go`), inline poll (`inline.go`), and the `goexec` Go-M executor — are *scheduler*
  properties, OS-neutral by construction. **fd fusion** (the `poll.go` pump +
  `netpoll_linkname.go` + `reactor.rs`) is the only OS-bound piece: it sources readiness
  from Go's netpoller's *readiness* model (epoll/kqueue), which **Windows netpoll (IOCP,
  completion-based) does not provide**. So the gate is `//go:build unix`, not a new tag.
- **`goexec` was already portable — that was the tell.** `goexec.rs` uses the platform-
  abstracted `Doorbell` (pipe on Windows), no `RawFd`; only `reactor.rs` is `std::os::unix`.
  So the executor half of the fast path needed *no* Rust change beyond gating `reactor`,
  `go_read_pipe`, `sable_spawn_read_pipe`, and `sable_fd_ready` to `#[cfg(all(feature =
  "fast", unix))]`.
- **Files that mixed both halves had to be split.** `bridge_fast.go` (kept the OS-neutral
  `RustAwaitsGo` await; moved the pipe/eventfd plumbing to `bridge_fusion_unix.go`),
  `api_fast.go` (→ `api_fusion_unix.go` for `ReadPipeViaRust`/`CountEpollFds`),
  `trampoline.go` (kept `noopAsm`/`asmcgocall`; moved `fdReady` → `trampoline_fusion_unix.go`,
  because its C helper takes the address of `sable_fd_ready`, which the Windows lib no longer
  exports). Same split for the `sable_safe` variant. Tests split the same way
  (`sable_fusion_unix_test.go`, `canary_fusion_unix_test.go`; `robust`/`stress` are wholly
  fusion so went `&& unix`; `raiseFDLimit` got a `!unix` no-op — Windows has no
  `RLIMIT_NOFILE`).
- **`TestCanaryAsmcgocall` is the Windows ABI cert.** It is already `!sable_safe &&
  !sable_portable` (OS-neutral), so it runs on Windows and directly certifies that the
  asmcgocall g0-switch + arg-passing works there — the same "scheduler property, not OS
  property" claim, now under test. The atomics + gopark/goready canaries join it; only the
  netpoll canary is Unix-gated.
- **Validated locally to the link boundary; runtime is a CI job.** With mingw-w64 + the
  rustup `x86_64-pc-windows-gnu` std, the fast staticlib builds and the fast Go **test
  binary, demo, safe variant, static binary, and all packages link** for `windows/amd64`;
  `nm` confirms `sable.pump`/`sable.fdReady`/`sable_fd_ready` are **absent** and
  `asmcgocall`/`gopark`/`goready`/`awaitGoExec` **present**. Native darwin fast+safe stay
  green under `-race`; portable still links. **Runtime** is left to the new native
  **`verify-windows-fast`** CI job (builds the fast lib into its own `target-win-fast` dir so
  it never clobbers the portable lib, runs the OS-neutral suite + canaries) — because this
  arm64 box's Docker **amd64 emulation is qemu-user**, which segfaults on complex binaries
  (`rustc --version` core-dumps under it, confirming the earlier finding), so Wine cannot run
  the exe locally, containerized or not. `make test-wine-fast` / `ci/wine-suite-fast.sh` is
  the Wine counterpart for boxes with working amd64 Wine.
- **Still open → done in the next entry: real fd fusion on Windows.** An IOCP/overlapped-read
  reactor (Go owns the read, hands the count to Rust). Landed below.

---

## fd fusion on Windows (IOCP)

The follow-on: make fd fusion — a Rust tokio task awaiting fd I/O driven by Go's single
event loop — work on Windows, where the loop is IOCP (completion), not epoll (readiness).

- **The contract inverts, and that's the whole design.** Unix: Go ships a readable *edge*,
  the Rust task does `libc::read`. Windows has no edge — an overlapped read completes only
  once bytes are already in a buffer. So **Go owns the overlapped read** through the runtime's
  single IOCP and delivers the *byte count* to Rust; the tokio task just awaits that one-shot
  completion (`rust/src/win_reactor.rs`, a completion analog of `reactor.rs`). tokio still
  owns no IOCP; Go's netpoller stays the sole event loop. Even the rejected "inject our own
  OVERLAPPED via `runtime_pollOpen`" alternative collapses to Go-owns-the-read (the kernel
  fills the buffer before the wakeup), so it bought only fragility.
- **The linchpin was a `go1.26` property, confirmed by source before writing a line.**
  `os.NewFile` → `newFile(kindNewFile)` calls `windows.IsNonblock`, which in go1.26 does a
  **kernel `NtQueryInformationFile(FileModeInformation)` query** (not an "os-created handles"
  set lookup, as older Go did). So a raw `CreateNamedPipeW(FILE_FLAG_OVERLAPPED)` handle is
  detected non-blocking → `pfd.Init(pollable=true)` → registered with the runtime IOCP →
  `File.Read` parks in `runtime_pollWait`. (`syscall.CreatePipe`/`os.Pipe` make *synchronous*
  handles, hence the named-pipe workaround via raw `kernel32` — no `x/sys` dep.)
- **Spike-first, because only the native runner can prove it.** The arm64 box can't run the
  binaries (qemu-user segfaults). PR 1 was Go-only: `overlappedPipe()` + a Toolhelp
  thread-counter + three tests, gated on the single unknown — does `os.NewFile` route the
  overlapped handle through the IOCP, or fall back to a blocking thread-per-read? The
  `TestSpikeBoundedThreads` result was decisive: **base=16 peak=16 delta=0 under 256 concurrent
  in-flight reads** — the thread count did not move, i.e. all reads park in the one IOCP.
  Thread-per-read would have added ~256. Threshold then tightened to `conc/2`.
- **PR 2 built the feature on that gate.** `win_reactor` (regid → result+waker, arm-then-check
  race guard, registration inserted *before* the task spawns so a completion racing the first
  poll never misses), two exports (`sable_spawn_read_handle` / `sable_fd_read_complete`), and
  `bridge_fusion_windows.go` (`readPipeViaRust` = overlapped pipe + writer + `os.NewFile` read
  pump + `awaitViaPark`). Every Go→Rust hop is an ordinary cgo call from a valid Go M; the
  tokio executor pthread only touches `Inner::complete`. **Go owns both pipe ends** and holds
  no Rust-side fd, so the Unix fd-reuse-race dance is simply absent — `os.File.Close` sequences
  IOCP-deregister then CloseHandle. `park.go`/`bridge.go`/`Inner::complete` were reused verbatim.
- **Still open (smaller now).** Delivering the read *bytes* (not just the count) to Rust;
  porting the `rust_awaits_go` eventfd value channel (so `RustAwaitsGo` returns 7 on Windows);
  cancellation-integrating the read pump on shutdown (the Unix `read_pipe` demo path isn't
  either); sockets/duplex.

---

## Standing project facts

- `go.mod` pins `toolchain go1.26.4`; **any Go upgrade is an ABI re-audit**. The
  linknamed seams are certified per-version by `make abi-check` (guard fails
  closed; override `SABLE_ALLOW_UNVERIFIED_GO=1` only to certify).
- `go.mod`'s `github.com/ihciah/rust2go` require is used **only** under
  `-tags sable_rust2go`, so `go mod tidy` must run with that tag or it drops the
  line.
- Building the core Rust lib (`--no-default-features`) to the **default** target
  dir clobbers the fast `libsable.a`; portable/http/rust2go builds each use a
  separate `--target-dir`.
- `go vet ./...` reports `possible misuse of unsafe.Pointer` notes in
  `park.go`/`trampoline.go` and the handle/stream tests (`handle_test.go`,
  `stream_test.go`) — intentional `unsafe.Pointer(uintptr)` on the gopark/asmcgocall
  glue and on deref of stable Rust-owned handle pointers; `go test`'s vet subset
  does not fail on them (the suites pass under `-race`).
- **The `main` branch ruleset requires verified signatures.** Commit signing is
  configured (`gpg.format=ssh`, `user.signingkey=~/.ssh/id_ed25519`) but
  `commit.gpgsign` is **off**, so commit with `-S` (or the push is rejected with
  `GH013`). The key is passphrase-protected; sign from an interactive shell (or with
  it loaded in `ssh-agent`). The remote is SSH; `gh` is authed with `repo` scope, so
  an HTTPS push via `git -c credential.helper='!gh auth git-credential' push
  https://github.com/moriyoshi/sable.git main` also works once the commit is signed.
- **Open follow-ups (not blocking):** replace the `internal/runtime/atomic.*`
  linknames in `park.go` with our own amd64/arm64 asm stubs for
  `Cas`/`Xchg`/`Store64` (Go tip dropped their linkname pushes — see the go-tip
  tripwire note above), **before** that change ships in a stable Go and breaks the
  default `-checklinkname=1` build; macOS certification on real hardware;
  **Windows fd fusion on IOCP** (the crossing/await/goexec half of the fast path
  now runs on `windows/amd64` — see "Fast crossing on Windows"; what remains is the
  IOCP/overlapped-read reactor); non-Linux `/proc` guards for the fast-only
  stress/robust suites; inline under `Handle::enter()` (M8) + pooled waker
  registry (M9); a typed codegen layer over the byte Call transport; the reverse
  direction (Rust awaiting a Go handler).
