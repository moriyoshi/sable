//go:build sable_rust2go

// Command rust2go-real wires in the REAL rust2go crate: a typed request crosses
// via rust2go's g2r cgo shim to kick off an async task on sable's fused runtime,
// whose completion resumes the parked goroutine with the typed result. Build/run
// via `make run-rust2go` (which sets GODEBUG=cgocheck=0,invalidptr=0 as rust2go
// requires). See examples/rust2go.md and real.go.
package main

import "github.com/moriyoshi/sable"

func main() {
	sable.Init()
	runDemo()
	sable.Shutdown()
}
