package audit

import (
	"bytes"
	"testing"
)

// FuzzRedact: after redaction no Password(554) or RawData(96) field may carry
// anything but the mask, wherever it sits in the message, and no other field may
// be lost or reordered.
func FuzzRedact(f *testing.F) {
	f.Add([]byte("8=FIX.4.4\x019=5\x0135=A\x01554=hunter2\x0196=deadbeef\x0110=000\x01"))
	f.Add([]byte("554=x"))
	f.Add([]byte("\x01554=\x01\x0196=a=b\x01"))
	f.Add([]byte("1554=keep\x015540=keep\x01960=keep\x01"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		out := Redact(raw)
		in, outs := bytes.Split(raw, []byte{1}), bytes.Split(out, []byte{1})
		if len(in) != len(outs) {
			t.Fatalf("field count changed: %d -> %d", len(in), len(outs))
		}
		for i, field := range outs {
			tag, _, _ := bytes.Cut(field, []byte("="))
			sensitive := bytes.Equal(tag, []byte("554")) || bytes.Equal(tag, []byte("96"))
			if sensitive && bytes.Contains(field, []byte("=")) && !bytes.Equal(field[len(tag)+1:], []byte("***")) {
				t.Fatalf("field %d leaked: %q", i, field)
			}
			if !sensitive && !bytes.Equal(field, in[i]) {
				t.Fatalf("non-secret field %d altered: %q -> %q", i, in[i], field)
			}
		}
	})
}
