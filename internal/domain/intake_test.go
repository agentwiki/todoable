package domain

import (
	"bytes"
	"testing"
)

func TestCanonical(t *testing.T) {
	for _, raw := range []string{`{"b":2,"a":1.0}`, `{ "a":1,"b":2 }`} {
		b, e := Canonical([]byte(raw))
		if e != nil || string(b) != `{"a":1,"b":2}` {
			t.Fatalf("%s %v", b, e)
		}
	}
	for _, raw := range []string{`{"a":1,"a":2}`, `{"n":9007199254740992}`, `{"n":1e100}`, `{"s":"\ud800"}`, `{} {}`, string([]byte{'"', 255, '"'})} {
		if _, e := Canonical([]byte(raw)); e == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}
func FuzzCanonical(f *testing.F) {
	for _, s := range []string{`{}`, `{"x":1.0}`, `{"x":"\ud800"}`, `{"x":1,"x":2}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 100000 {
			t.Skip()
		}
		c, e := Canonical(b)
		if e != nil {
			return
		}
		again, e := Canonical(c)
		if e != nil || !bytes.Equal(c, again) {
			t.Fatalf("canonicalization not idempotent: %q %v", c, e)
		}
	})
}
