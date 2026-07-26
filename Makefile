# Sable — build orchestration: cargo (staticlib) THEN go build -> single binary.
#
# SABLE_LDFLAGS: extra -ldflags passed to go build/test. Empty by default; the
# chosen linkname seams (gopark/goready/poll_runtime_poll*) link under the
# default -checklinkname=1 on go1.26.4, so no flag is required. Flip to
# `-checklinkname=0` only if a milestone reaches raw netpoll* symbols.

RUST_DIR   := rust
RUST_LIB   := $(RUST_DIR)/target/release/libsable.a
BIN        := sable
# The importable library is the repo root package; the demo binary is cmd/sable-demo.
GO_SRC     := $(wildcard *.go) $(wildcard cmd/sable-demo/*.go)
RUST_SRC   := $(wildcard $(RUST_DIR)/src/*.rs) $(RUST_DIR)/Cargo.toml
SABLE_LDFLAGS ?=
GO_LDFLAGS := $(if $(SABLE_LDFLAGS),-ldflags="$(SABLE_LDFLAGS)",)

# Cross-compilation. NATIVE builds on any 64-bit arch (amd64, arm64, riscv64,
# ppc64le, s390x, ...) need none of this — just `make`. To cross-build from one
# host to another, provide a Rust target triple, a GOARCH, and a C cross-compiler
# (zig makes a fine one, no install of a gcc cross-toolchain required):
#   make cross RUST_TARGET=x86_64-unknown-linux-gnu GOARCH=amd64 \
#              CROSS_CC="zig cc -target x86_64-linux-gnu"
RUST_TARGET ?=
GOARCH      ?=
CROSS_CC    ?=

# Real-world scenario: reqwest (client) + axum (server) on a shared-completion
# io-enabled runtime. Heavy deps, opt-in. Built into a SEPARATE target dir so the
# http-enabled lib (which references the sable_go_double export) never clobbers
# the lean core lib; CGO_LDFLAGS points the linker at it (searched first).
HTTP_TARGET_DIR := $(RUST_DIR)/target-http
HTTP_CGO_LDFLAGS := -L$(CURDIR)/$(HTTP_TARGET_DIR)/release

.PHONY: all rust clean test test-safe verify-all bench run cross http run-http test-http abi-check portable test-portable loom fuzz miri test-pipe rust2go run-rust2go test-rust2go gen-rust2go test-multithread test-qemu-amd64 test-wine test-wine-fast

all: $(BIN)

# Certify the current Go toolchain's internal ABI. Run this before adding a Go
# version to SupportedGoVersions (guard.go). Green => the linkname/asm surface
# still works on this release.
abi-check: $(RUST_LIB)
	@echo ">> behavioral canaries"
	SABLE_ALLOW_UNVERIFIED_GO=1 go test -race -count=1 -run '^TestCanary' -v .
	@echo ">> linknamed symbols present in the linked binary"
	@go build -o $(BIN) ./cmd/sable-demo
	@for s in runtime.gopark runtime.goready runtime.asmcgocall \
	          'internal/poll.runtime_pollWait' 'internal/runtime/atomic.Cas'; do \
	  go tool nm $(BIN) | grep -qE " $$s\$$" && echo "  ok   $$s" || { echo "  MISSING $$s"; exit 1; }; \
	done
	@echo ">> $$(go env GOVERSION): PASS — safe to add to SupportedGoVersions"

$(RUST_LIB): $(RUST_SRC)
	cargo build --release --manifest-path $(RUST_DIR)/Cargo.toml

rust: $(RUST_LIB)

$(BIN): $(RUST_LIB) $(GO_SRC)
	go build $(GO_LDFLAGS) -o $(BIN) ./cmd/sable-demo

run: $(BIN)
	./$(BIN)

# Go's race detector is TSan-based; per the plan we validate the Go half under
# -race with the Rust half built normally (Rust TSan/ASan is a separate run).
test: $(RUST_LIB)
	go test $(GO_LDFLAGS) -race ./...

# The sable_safe variant (cgo crossing instead of the asmcgocall fast path).
test-safe: $(RUST_LIB)
	go test $(GO_LDFLAGS) -race -tags sable_safe ./...

# Everything CI runs on a certified toolchain, in one command.
verify-all:
	$(MAKE) abi-check
	$(MAKE) test
	$(MAKE) test-safe
	$(MAKE) test-pipe
	$(MAKE) test-http

# Runs the separated public-API microbenchmarks (bench/) plus the in-package
# internal benchmarks (inline/goexec/stress).
bench: $(RUST_LIB)
	go test $(GO_LDFLAGS) -run='^$$' -bench=. -benchmem . ./bench/...

cross:
	@test -n "$(RUST_TARGET)" -a -n "$(GOARCH)" -a -n "$(CROSS_CC)" || \
	  { echo "usage: make cross RUST_TARGET=<triple> GOARCH=<arch> CROSS_CC=<cc>"; exit 2; }
	rustup target add $(RUST_TARGET) 2>/dev/null || true
	cargo build --release --manifest-path $(RUST_DIR)/Cargo.toml --target $(RUST_TARGET)
	# CGO_LDFLAGS is searched before the in-file -L, so the target's libsable.a
	# wins over any native one — no lib swapping needed.
	CGO_ENABLED=1 GOARCH=$(GOARCH) CC="$(CROSS_CC)" \
	  CGO_LDFLAGS="-L$(CURDIR)/$(RUST_DIR)/target/$(RUST_TARGET)/release" \
	  go build $(GO_LDFLAGS) -o $(BIN)-$(GOARCH) ./cmd/sable-demo
	@echo "built $(BIN)-$(GOARCH) ($(GOARCH))"

# Emulated amd64 verification (R-emu, LOCAL). Runs the suite as amd64 under
# qemu-user — the reproducible form of the README's "amd64 run under qemu-x86_64"
# claim, for exercising amd64 from an arm64 dev box (CI already covers amd64
# natively, so there is no CI job for this). Because rustc/LLVM segfault under
# qemu-user, the toolchain image (ci/Dockerfile.emu) is NATIVE-arch and
# CROSS-compiles to amd64 with zig; only the resulting test binaries run emulated.
# Steps: install the qemu-user-static binfmt handlers (tonistiigi/binfmt — the
# maintained, multi-arch successor to a bare `multiarch/qemu-user-static --reset`,
# which is amd64-only and so cannot self-register from an arm64 host; usually a
# no-op on Docker Desktop), build the image, then copy the WORKING TREE (minus the
# host's target dirs / .git, so native artifacts never leak in) into a throwaway
# dir and run ci/emu-suite.sh. Race detector is off there (qemu).
EMU_IMAGE    ?= sable-emu
# Pinned by digest for reproducibility (tonistiigi/binfmt:qemu-v9.2.2). Bump
# deliberately — a floating tag would change the emulator with no diff.
BINFMT_IMAGE ?= tonistiigi/binfmt@sha256:1b804311fe87047a4c96d38b4b3ef6f62fca8cd125265917a9e3dc3c996c39e6
test-qemu-amd64:
	docker run --privileged --rm $(BINFMT_IMAGE) --install amd64
	docker build -f ci/Dockerfile.emu -t $(EMU_IMAGE) ci
	docker run --rm -v $(CURDIR):/src:ro $(EMU_IMAGE) bash -euc '\
	  mkdir -p /work && cd /src && \
	  tar --exclude=./.git --exclude=./rust/target --exclude="./rust/target-*" -cf - . | (cd /work && tar -xf -) && \
	  cd /work && bash ci/emu-suite.sh'

# Windows/amd64 via Wine (LOCAL). Cross-compiles the portable Sable to a windows
# .exe (Rust staticlib + cgo through mingw-w64) and runs its suite under Wine —
# how the Windows portable fallback (cross-platform anonymous-pipe doorbell) is
# exercised without a Windows machine. The build is native (rustc/go cross-
# compile); only the .exe runs under Wine — native on an amd64 host, via Rosetta
# on Apple Silicon. The verify-windows-wine CI job runs the same ci/wine-suite.sh.
# Requires on PATH: cargo + the x86_64-pc-windows-gnu rust target
# (`rustup target add x86_64-pc-windows-gnu`), x86_64-w64-mingw32-gcc, go, wine.
test-wine:
	@command -v x86_64-w64-mingw32-gcc >/dev/null || { echo "need mingw-w64: brew install mingw-w64  |  apt-get install gcc-mingw-w64-x86-64"; exit 2; }
	@command -v wine64 >/dev/null || command -v wine >/dev/null || { echo "need wine: brew install --cask wine-stable  |  apt-get install wine64"; exit 2; }
	bash ci/wine-suite.sh

# Windows/amd64 FAST build via Wine (LOCAL). Same cross-compile + Wine model as
# test-wine, but builds the default-feature ("fast") lib and runs the OS-neutral
# fast suite — the "fast crossing on Windows" (asmcgocall crossing, gopark/goready
# await, inline poll, goexec). fd fusion (the single-epoll pump) is Unix-only and
# compiled out. The verify-windows-fast CI job is the authoritative native gate.
test-wine-fast:
	@command -v x86_64-w64-mingw32-gcc >/dev/null || { echo "need mingw-w64: brew install mingw-w64  |  apt-get install gcc-mingw-w64-x86-64"; exit 2; }
	@command -v wine64 >/dev/null || command -v wine >/dev/null || { echo "need wine: brew install --cask wine-stable  |  apt-get install wine64"; exit 2; }
	bash ci/wine-suite-fast.sh

http:
	cargo build --release --manifest-path $(RUST_DIR)/Cargo.toml --features http --target-dir $(HTTP_TARGET_DIR)
	CGO_LDFLAGS="$(HTTP_CGO_LDFLAGS)" go build -tags sable_http $(GO_LDFLAGS) -o $(BIN)-http ./examples/http

run-http: http
	./$(BIN)-http

test-http:
	cargo build --release --manifest-path $(RUST_DIR)/Cargo.toml --features http --target-dir $(HTTP_TARGET_DIR)
	CGO_LDFLAGS="$(HTTP_CGO_LDFLAGS)" go test -tags sable_http $(GO_LDFLAGS) -race -run TestReqwestAxumFusion -v ./examples/http/...

# Pure-tokio baseline (no Go / fusion) — run alongside `SABLE_STRESS=1 go test
# -bench=Fusion` and `make bench` to quantify the fusion overhead.
# Portable ZERO-LINKNAME fallback: core lib (Rust --no-default-features) + the
# channel-based await backend (-tags sable_portable). Works on ANY Go toolchain
# (no internal ABI), at the cost of the advanced paths (single-epoll pump,
# goexec, inline). This is the insurance for an uncertified / future Go release.
PORT_TARGET_DIR := $(RUST_DIR)/target-portable
PORT_CGO_LDFLAGS := -L$(CURDIR)/$(PORT_TARGET_DIR)/release
portable:
	cargo build --release --manifest-path $(RUST_DIR)/Cargo.toml --no-default-features --features demo --target-dir $(PORT_TARGET_DIR)
	CGO_LDFLAGS="$(PORT_CGO_LDFLAGS)" go build -tags sable_portable -o $(BIN)-portable ./cmd/sable-demo
test-portable:
	cargo build --release --manifest-path $(RUST_DIR)/Cargo.toml --no-default-features --features demo --target-dir $(PORT_TARGET_DIR)
	CGO_LDFLAGS="$(PORT_CGO_LDFLAGS)" go test -tags sable_portable -race ./...

# Cross-platform doorbell (R6d): force the self-pipe doorbell (the macOS/BSD
# primitive) on Linux and run the full race suite, proving the abstraction works
# on a box where the race detector is available.
# SABLE_PIPE_DOORBELL is read by Rust at process start; it does not key Go's test
# cache, so -count=1 forces an actual run through the self-pipe path.
test-pipe: $(RUST_LIB)
	SABLE_PIPE_DOORBELL=1 go test $(GO_LDFLAGS) -race -count=1 ./...

# Real rust2go integration (examples/rust2go.md): rust2go's g2r typed cgo crossing
# kicks off an async task on Sable's FUSED runtime; the result comes back through
# Sable's completion doorbell. Opt-in (own target dir; heavy Go-module + Rust dep).
# rust2go passes Go pointers through its ref structs, so it REQUIRES the relaxed
# cgo checks below (same as rust2go's own README/CI).
R2G_TARGET_DIR := $(RUST_DIR)/target-rust2go
R2G_CGO_LDFLAGS := -L$(CURDIR)/$(R2G_TARGET_DIR)/release
R2G_GODEBUG := GODEBUG=cgocheck=0,invalidptr=0
# Regenerate the Go binding from the Rust trait (needs `cargo install rust2go-cli`),
# re-prepending the isolation build tag rust2go-cli does not emit.
R2G_GEN := examples/rust2go-real/r2g_gen.go
gen-rust2go:
	rust2go-cli --src $(RUST_DIR)/src/r2g.rs --dst $(R2G_GEN) --without-main
	printf '//go:build sable_rust2go\n\n%s' "$$(cat $(R2G_GEN))" > $(R2G_GEN).tmp
	mv $(R2G_GEN).tmp $(R2G_GEN)
rust2go:
	cargo build --release --manifest-path $(RUST_DIR)/Cargo.toml --features rust2go --target-dir $(R2G_TARGET_DIR)
	CGO_LDFLAGS="$(R2G_CGO_LDFLAGS)" go build -tags sable_rust2go -o $(BIN)-rust2go ./examples/rust2go-real
run-rust2go: rust2go
	$(R2G_GODEBUG) ./$(BIN)-rust2go
test-rust2go:
	cargo build --release --manifest-path $(RUST_DIR)/Cargo.toml --features rust2go --target-dir $(R2G_TARGET_DIR)
	CGO_LDFLAGS="$(R2G_CGO_LDFLAGS)" $(R2G_GODEBUG) go test -tags sable_rust2go -run TestRust2GoReal -v ./examples/rust2go-real/...

# --- R5 verification tooling ---------------------------------------------------
# Loom: exhaustively model-check the await-slot park/deliver protocol (every
# interleaving of gopark's parkCommit CAS vs deliverCompletion's Xchg+goready).
loom:
	cd $(RUST_DIR) && RUSTFLAGS="--cfg loom" cargo test --test loom_wakeup -- --nocapture

# Fuzz the byte-buffer Call FFI marshalling (ptr/len crossing, GoBytes copy,
# Rust-side free) with arbitrary inputs. FUZZTIME overridable (default 30s).
FUZZTIME ?= 30s
fuzz: $(RUST_LIB)
	go test -run '^$$' -fuzz='^FuzzCall$$' -fuzztime=$(FUZZTIME) .

# Miri: UB-check the raw-pointer lifecycle behind the Call ABI (into_raw / read /
# free). Needs nightly + the miri component: rustup +nightly component add miri.
# Runs on the CORE lib (--no-default-features) so no Go externs are referenced.
miri:
	cargo +nightly miri test --manifest-path $(RUST_DIR)/Cargo.toml --no-default-features call_unsafe_tests

bench-pure:
	cargo build --release --manifest-path $(RUST_DIR)/Cargo.toml --features http --bin pure_bench --target-dir $(HTTP_TARGET_DIR)
	$(HTTP_TARGET_DIR)/release/pure_bench

# S-4.2: (1) empirically validate that an IO-disabled multi-thread executor
# creates NO epoll (Rust s4_epoll tests, with an enable_all+socket control), then
# (2) end-to-end — build the runtime on that executor (-tags sable_multithread)
# and assert the fused Go program still holds exactly one epoll while the
# handle/stream paths work. Green => real parallelism for CPU-heavy handlers with
# the single-epoll invariant preserved.
MT_TARGET_DIR := $(RUST_DIR)/target-multithread
MT_CGO_LDFLAGS := -L$(CURDIR)/$(MT_TARGET_DIR)/release
test-multithread:
	cargo test --release --manifest-path $(RUST_DIR)/Cargo.toml --features multithread s4_epoll -- --test-threads=1
	cargo build --release --manifest-path $(RUST_DIR)/Cargo.toml --features multithread --target-dir $(MT_TARGET_DIR)
	CGO_LDFLAGS="$(MT_CGO_LDFLAGS)" go test -tags sable_multithread -race -count=1 -run 'TestMultithread|TestCallHandle|TestStream' .

clean:
	cargo clean --manifest-path $(RUST_DIR)/Cargo.toml
	rm -rf $(HTTP_TARGET_DIR) $(PORT_TARGET_DIR) $(R2G_TARGET_DIR)
	rm -f $(BIN) $(BIN)-*
