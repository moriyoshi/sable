//go:build !sable_extern_lib

package sable

// link_default.go — default cgo link directive (S-5): link sable's own staticlib
// libsable.a from rust/target/release. The portable/http/rust2go builds prepend
// their own -L via CGO_LDFLAGS (searched first) but still resolve -lsable here.

// #cgo LDFLAGS: -L${SRCDIR}/rust/target/release -lsable -lgcc_s -lutil -lrt -lpthread -lm -ldl
import "C"
