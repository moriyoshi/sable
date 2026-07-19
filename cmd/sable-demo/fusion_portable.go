//go:build sable_portable

package main

import "fmt"

// fusionDemo (portable build): the reactor/pump/goexec/inline paths are compiled
// out, so only the core channel-based "Go awaits Rust" is available.
func fusionDemo() bool {
	fmt.Println("  (portable zero-linkname build: reactor/pump, goexec, inline disabled;")
	fmt.Println("   core Go-awaits-Rust via channels only — works on any Go toolchain)")
	return true
}
