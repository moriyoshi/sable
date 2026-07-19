module github.com/moriyoshi/sable

go 1.26

// Pin the exact toolchain: this PoC reaches internal runtime seams via
// //go:linkname (gopark/goready/poll_runtime_poll*) whose ABI is version-
// specific. Any Go upgrade is an ABI re-audit, not a routine bump.
toolchain go1.26.4

require github.com/ihciah/rust2go v0.0.0-20260706074211-1c851bd436aa // indirect
