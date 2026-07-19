//go:build !sable_http

package main

import "fmt"

// This example needs the Rust lib built with --features http (reqwest + axum),
// so it is behind the sable_http build tag. A plain `go build ./...` compiles
// this stub instead.
func main() {
	fmt.Println("http example: build with `make run-http` (-tags sable_http, " +
		"Rust --features http).")
}
