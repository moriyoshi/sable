package sable

// guard.go — the durability foundation for depending on Go's internal ABI.
//
// Sable reaches runtime internals via //go:linkname (gopark/goready,
// poll_runtime_poll*, internal/runtime/atomic.*) and runtime.asmcgocall. Those
// are unstable across Go releases. The commitment to track them is only
// tractable if breakage is detected automatically, so:
//
//   * SupportedGoVersions lists toolchains whose internal ABI has been VERIFIED
//     by the canary suite (canary_test.go). It is the single source of truth.
//   * requireVerifiedGo() fails closed on anything else — running the fragile
//     paths on an unverified toolchain risks silent corruption, so we refuse.
//   * `make abi-check` runs the canaries (with the override) against the current
//     toolchain; if green, add it to the list below. That is the whole process
//     of certifying a new Go release.
//
// (A library form would surface this as an error from the runtime constructor
// rather than a panic; sable is a single binary, so a fail-closed panic in the
// constructor is the honest default.)

import (
	"os"
	"runtime"
	"strings"
)

// SupportedGoVersions: toolchains certified via `make abi-check`.
var SupportedGoVersions = []string{
	"go1.26.4",
}

// Verified reports whether the running toolchain's internal ABI is certified.
func Verified() bool {
	v := runtime.Version()
	for _, s := range SupportedGoVersions {
		if s == v {
			return true
		}
	}
	return false
}

// requireVerifiedGo aborts on an uncertified toolchain unless explicitly
// overridden (SABLE_ALLOW_UNVERIFIED_GO=1, which `make abi-check` sets to run
// the canaries that certify a new version).
func requireVerifiedGo() {
	// The portable (zero-linkname) build is safe on any toolchain.
	if portableBuild || Verified() || os.Getenv("SABLE_ALLOW_UNVERIFIED_GO") != "" {
		return
	}
	panic("sable: uncertified Go toolchain " + runtime.Version() +
		" — internal ABI (linkname/asm) may be invalid. Certified: " +
		strings.Join(SupportedGoVersions, ", ") +
		". Run `make abi-check`; if it passes, add this version to " +
		"SupportedGoVersions. To proceed anyway, set SABLE_ALLOW_UNVERIFIED_GO=1.")
}
