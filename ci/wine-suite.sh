#!/usr/bin/env bash
# Build the portable Sable for windows/amd64 and run its test suite under Wine.
#
# The Rust staticlib and the cgo Go test binary are CROSS-compiled (native rustc +
# go, mingw-w64 as the cgo C compiler) — only the resulting .exe runs under Wine,
# which on an amd64 host executes it natively (no CPU emulation). This is how the
# Windows portable fallback (cross-platform anonymous-pipe doorbell) is exercised
# without a Windows runner. Race detector is not available on this path.
#
# Requires on PATH: cargo + the x86_64-pc-windows-gnu rust target,
# x86_64-w64-mingw32-gcc (mingw-w64), go, and wine64. Run from the repo root.
set -euxo pipefail

TARGET=x86_64-pc-windows-gnu

echo "== Rust portable staticlib for ${TARGET} =="
cargo build --release --manifest-path rust/Cargo.toml \
  --no-default-features --features demo --target "${TARGET}"

echo "== cross-compile the portable Go test binary (cgo via mingw, static) =="
export CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc
export CGO_LDFLAGS="-L$PWD/rust/target/${TARGET}/release"
# Static-link the mingw runtime so the .exe is self-contained under Wine.
go test -tags sable_portable -c -o sable_win_test.exe \
  -ldflags '-linkmode external -extldflags "-static"' .

echo "== run the portable suite under Wine =="
export WINEDEBUG="${WINEDEBUG:--all}"           # quiet Wine's own noise
export WINEPREFIX="${WINEPREFIX:-$PWD/.wineprefix}"
WINE="${WINE:-$(command -v wine64 || command -v wine || true)}"
[ -n "$WINE" ] || { echo "no wine/wine64 on PATH — install the 'wine' package"; exit 1; }
"$WINE" ./sable_win_test.exe -test.v -test.count=1
echo "== OK: portable suite passed under Wine (windows/amd64) =="
