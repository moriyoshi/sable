#!/usr/bin/env bash
# Build the FAST Sable for windows/amd64 and run its OS-neutral suite under Wine.
#
# The counterpart to ci/wine-suite.sh (which exercises the PORTABLE fallback).
# This one builds the default-feature ("fast") staticlib and runs the fast Go
# suite — i.e. the "fast crossing on Windows": the asmcgocall crossing,
# gopark/goready await, inline poll, and the goexec executor. fd fusion (the
# single-shared-epoll pump) is Unix-only (Windows netpoll is IOCP, not
# readiness-based), so those tests are compiled out here by build tag; the native
# verify-windows-fast CI job is the authoritative runtime gate.
#
# The Rust staticlib and the cgo Go test binary are CROSS-compiled (native rustc +
# go, mingw-w64 as the cgo C compiler); only the resulting .exe runs under Wine,
# which on an amd64 host executes it natively (via Rosetta on Apple Silicon).
# Race detector is not available on this path.
#
# Requires on PATH: cargo + the x86_64-pc-windows-gnu rust target,
# x86_64-w64-mingw32-gcc (mingw-w64), go, and wine64. Run from the repo root.
set -euxo pipefail

TARGET=x86_64-pc-windows-gnu
# Own target dir so the fast staticlib never clobbers the portable one (building
# core with --no-default-features to the same dir would — see JOURNAL "Standing
# project facts").
TARGET_DIR=rust/target-win-fast

echo "== Rust FAST staticlib for ${TARGET} =="
cargo build --release --manifest-path rust/Cargo.toml \
  --target "${TARGET}" --target-dir "${TARGET_DIR}"

echo "== cross-compile the fast Go test binary (cgo via mingw, static) =="
export CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc
export CGO_LDFLAGS="-L$PWD/${TARGET_DIR}/${TARGET}/release"
# Static-link the mingw runtime so the .exe is self-contained under Wine.
go test -c -o sable_win_fast_test.exe \
  -ldflags '-linkmode external -extldflags "-static"' .

echo "== run the fast suite under Wine =="
export WINEDEBUG="${WINEDEBUG:--all}"           # quiet Wine's own noise
export WINEPREFIX="${WINEPREFIX:-$PWD/.wineprefix}"
WINE="${WINE:-$(command -v wine64 || command -v wine || true)}"
[ -n "$WINE" ] || { echo "no wine/wine64 on PATH — install the 'wine' package"; exit 1; }
"$WINE" ./sable_win_fast_test.exe -test.v -test.count=1
echo "== OK: fast suite passed under Wine (windows/amd64) =="
