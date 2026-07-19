//go:build !sable_rust2go

package main

import "fmt"

// This example needs the real rust2go crate and the Rust lib built with
// --features rust2go, so it is behind the sable_rust2go build tag. A plain
// `go build ./...` compiles this stub instead.
func main() {
	fmt.Println("rust2go-real: build with `make run-rust2go` (-tags sable_rust2go, " +
		"GODEBUG=cgocheck=0,invalidptr=0). See examples/rust2go.md.")
}
