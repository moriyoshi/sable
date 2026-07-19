//go:build !sable_portable

// This assembly file is intentionally empty. Its mere presence lets the Go
// compiler accept the bodyless function declarations in netpoll_linkname.go
// (and later park.go), which are bound to runtime-internal symbols via
// //go:linkname rather than a Go body.
