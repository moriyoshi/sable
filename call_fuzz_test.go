package sable

import (
	"bytes"
	"strings"
	"testing"
)

// FuzzCall drives arbitrary request bytes through the byte-buffer FFI
// marshalling (ptr/len across the boundary, GoBytes copy, Rust-side free) for
// the fast, non-suspending ops. Invariants: no crash/hang/leak, and the data
// ops round-trip correctly. Run: go test -run '^$' -fuzz=FuzzCall -fuzztime=20s
func FuzzCall(f *testing.F) {
	f.Add(uint32(0), []byte("hello"))
	f.Add(uint32(1), []byte(""))
	f.Add(uint32(2), []byte{0, 1, 2, 0xff})
	f.Add(uint32(3), []byte("boom"))
	f.Add(uint32(0), bytes.Repeat([]byte{0}, 4096))
	f.Fuzz(func(t *testing.T, op uint32, req []byte) {
		op = op % 4 // 0=echo 1=upper 2=len 3=fail — bounded, non-suspending
		got, err := Call(op, req)
		switch op {
		case OpEcho:
			if err != nil {
				t.Fatalf("echo err: %v", err)
			}
			if !bytes.Equal(got, req) {
				t.Fatalf("echo mismatch: got %d bytes want %d", len(got), len(req))
			}
		case OpUpper:
			if err != nil {
				t.Fatalf("upper err: %v", err)
			}
			if !bytes.Equal(got, asciiUpper(req)) {
				t.Fatalf("upper mismatch")
			}
		case OpLen:
			if err != nil {
				t.Fatalf("len err: %v", err)
			}
			if string(got) != itoa(len(req)) {
				t.Fatalf("len mismatch: got %q want %d", got, len(req))
			}
		case OpFail:
			if err == nil {
				t.Fatalf("fail op should error")
			}
		}
	})
}

// asciiUpper mirrors Rust's byte-wise to_ascii_uppercase (op 1's semantics):
// only ASCII a-z are shifted; all other bytes (incl. invalid UTF-8) pass through.
func asciiUpper(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		out[i] = c
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b strings.Builder
	var digits []byte
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	for i := len(digits) - 1; i >= 0; i-- {
		b.WriteByte(digits[i])
	}
	return b.String()
}
