//go:build sable_portable

package sable

// park_portable.go — the PORTABLE await backend: zero //go:linkname, zero asm.
// It uses only supported APIs (a buffered channel + the runtime's normal
// goroutine parking on a channel receive), so it works on ANY Go toolchain —
// the fallback that makes an uncertified/future Go version non-fatal.
//
// This is the M1 design: slower than the gopark/goready path (a channel op and
// a scheduler park via the public API instead of a direct scheduler primitive),
// but it depends on nothing internal. Selected with `-tags sable_portable`
// against the Rust core lib (`cargo build --no-default-features`).

import "sync"

const portableBuild = true

// awaits maps a token to the per-await delivery channel.
var awaits sync.Map // uint64 token -> chan uint64

// awaitViaPark: register a channel, kick off the Rust work, block on the channel
// until the dispatcher delivers. Register-before-spawn + buffered channel => no
// lost wakeup.
func awaitViaPark(spawn func(token uint64)) uint64 {
	token := tokenCtr.Add(1)
	ch := make(chan uint64, 1)
	awaits.Store(token, ch)
	spawn(token)
	return <-ch
}

// awaitViaParkTry is awaitViaPark for an admission that may be refused: spawn
// returns whether the task was admitted. On refusal NO completion will arrive
// on the channel, so we must not receive — we drop the slot and report false.
func awaitViaParkTry(spawn func(token uint64) bool) (uint64, bool) {
	token := tokenCtr.Add(1)
	ch := make(chan uint64, 1)
	awaits.Store(token, ch)
	if !spawn(token) {
		awaits.Delete(token)
		return 0, false
	}
	return <-ch, true
}

// deliverCompletion delivers a result to the awaiting goroutine's channel.
// Called from the dispatcher goroutine.
func deliverCompletion(token uint64, result uint64) {
	if v, ok := awaits.LoadAndDelete(token); ok {
		v.(chan uint64) <- result
	}
}
