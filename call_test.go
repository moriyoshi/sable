package sable

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

// TestCall exercises the generic byte-buffer API: transform ops, length-as-string,
// a genuinely-async handler (OpDelayEcho suspends on a timer), error propagation,
// and concurrency. Works under both backends.
func TestCall(t *testing.T) {
	cases := []struct {
		op       uint32
		req      []byte
		want     []byte
		wantErr  string
	}{
		{OpEcho, []byte("hello"), []byte("hello"), ""},
		{OpUpper, []byte("hello"), []byte("HELLO"), ""},
		{OpLen, []byte("hello"), []byte("5"), ""},
		{OpDelayEcho, []byte("async"), []byte("async"), ""},
		{OpEcho, nil, []byte{}, ""},
		{OpFail, []byte("x"), nil, "boom"},
		{99, []byte("x"), nil, "unknown op 99"},
	}
	for _, c := range cases {
		got, err := Call(c.op, c.req)
		if c.wantErr != "" {
			if err == nil || err.Error() != c.wantErr {
				t.Errorf("op %d: err=%v, want %q", c.op, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("op %d: unexpected err %v", c.op, err)
			continue
		}
		if !bytes.Equal(got, c.want) {
			t.Errorf("op %d: got %q, want %q", c.op, got, c.want)
		}
	}

	// Concurrent echo with unique payloads (detects cross-wired deliveries).
	var wg sync.WaitGroup
	errs := make(chan string, 1000)
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := []byte(fmt.Sprintf("msg-%d", i))
			got, err := Call(OpEcho, p)
			if err != nil || !bytes.Equal(got, p) {
				errs <- fmt.Sprintf("i=%d got=%q err=%v", i, got, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
