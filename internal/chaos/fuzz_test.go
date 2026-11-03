package chaos

import (
	"bytes"
	"testing"
)

// FuzzFramer: however a byte stream is chunked, the framer must emit the same
// messages, never panic, and never invent bytes.
func FuzzFramer(f *testing.F) {
	msg := []byte("8=FIX.4.4\x019=5\x0135=0\x0110=123\x01")
	f.Add(append(append([]byte{}, msg...), msg...), 3)
	f.Add([]byte("garbage"), 1)
	f.Add([]byte("10=\x0110=\x01"), 2)
	f.Fuzz(func(t *testing.T, stream []byte, chunk int) {
		if chunk < 1 {
			chunk = 1
		}
		var whole, chunked Framer
		want := whole.Push(stream)
		var got [][]byte
		for i := 0; i < len(stream); i += chunk {
			got = append(got, chunked.Push(stream[i:min(i+chunk, len(stream))])...)
		}
		if len(want) != len(got) {
			t.Fatalf("%d messages whole, %d chunked", len(want), len(got))
		}
		for i := range want {
			if !bytes.Equal(want[i], got[i]) {
				t.Fatalf("message %d differs", i)
			}
			MsgType(want[i]) // must not panic
		}
	})
}
