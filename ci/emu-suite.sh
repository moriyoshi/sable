#!/usr/bin/env bash
# Runs inside ci/Dockerfile.emu. Cross-compiles Sable for $TRIPLE with native
# rustc + go (fast, no emulation) using zig as the cgo cross C compiler, then runs
# the resulting test binaries — which binfmt transparently executes under
# qemu-user. Only Sable's own test binaries run emulated; the toolchains do not.
#
# Race detector is OFF: Go's TSan is unreliable under qemu-user (the native and
# macOS jobs are the -race gate). Overridable via TRIPLE / ZIG_TARGET / GOARCH.
set -euxo pipefail

TRIPLE="${TRIPLE:-x86_64-unknown-linux-gnu}"
ZIG_TARGET="${ZIG_TARGET:-x86_64-linux-gnu.2.36}" # glibc 2.36 == bookworm's libc6:amd64
export CGO_ENABLED=1 GOARCH="${GOARCH:-amd64}"
export CC="zig cc -target ${ZIG_TARGET}" CXX="zig c++ -target ${ZIG_TARGET}"

echo "== portable build (zero-linkname) — the OS/arch-portable core =="
cargo build --release --manifest-path rust/Cargo.toml \
  --no-default-features --features demo --target "$TRIPLE" --target-dir rust/target-portable
CGO_LDFLAGS="-L$PWD/rust/target-portable/$TRIPLE/release" \
  go test -tags sable_portable -count=1 ./...

echo "== fast build (default features: asmcgocall + linkname on the emulated arch) =="
cargo build --release --manifest-path rust/Cargo.toml --target "$TRIPLE"
CGO_LDFLAGS="-L$PWD/rust/target/$TRIPLE/release" \
  go test -count=1 .

echo "== OK: cross-built for $TRIPLE, ran under qemu-user =="
