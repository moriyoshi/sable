// Package bench holds sable's microbenchmarks that measure the public API as a
// consumer would — the raw Go->Rust crossing (cgo vs the asmcgocall fast path)
// and the end-to-end mutual-await round trip. They live outside the library so
// the core package ships without benchmark scaffolding. Benchmarks that measure
// unexported internals (the inline and goexec paths) necessarily stay in-package
// as _test.go files alongside the code they exercise.
//
// Run with: make bench
package bench
