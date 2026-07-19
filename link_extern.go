//go:build sable_extern_lib

package sable

// link_extern.go — embedder-owned staticlib (S-5). Built with -tags
// sable_extern_lib, sable's Go package contributes NO -lsable/-L of its own: the
// combined staticlib (imbh + sable runtime + registered handlers) is supplied by
// the embedder via CGO_LDFLAGS, e.g.
//
//   CGO_LDFLAGS="-L/path/to/binding -limbhgo" \
//     go build -tags sable_extern_lib ./...
//
// sable's Go code still calls the same `sable_*` symbols; they now resolve out of
// the embedder's combined lib instead of libsable.a. Only the system libs sable's
// Rust runtime needs remain here.

// #cgo LDFLAGS: -lgcc_s -lutil -lrt -lpthread -lm -ldl
import "C"
