package audit

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRedact(t *testing.T) {
	in := "8=FIX.4.4\x0135=A\x0196=deadbeef\x01553=key\x01554=hunter2\x0110=000\x01"
	got := string(Redact([]byte(in)))
	if strings.Contains(got, "deadbeef") || strings.Contains(got, "hunter2") {
		t.Errorf("secret leaked: %q", got)
	}
	if !strings.Contains(got, "96=***") || !strings.Contains(got, "554=***") || !strings.Contains(got, "553=key") {
		t.Errorf("unexpected redaction: %q", got)
	}
	plain := []byte("8=FIX.4.4\x0135=D\x0111=abc\x01")
	if string(Redact(plain)) != string(plain) {
		t.Error("message without secrets must be untouched")
	}
}

func TestChainAndResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Log("in", "S1", []byte("8=FIX.4.4\x0135=0\x01"))
		}()
	}
	wg.Wait()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := Verify(path); err != nil || n != 20 {
		t.Fatalf("verify: n=%d err=%v", n, err)
	}

	// Reopen and continue the same chain.
	l, err = Open(path, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	l.Log("out", "S1", []byte("8=FIX.4.4\x0135=0\x01"))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := Verify(path); err != nil || n != 21 {
		t.Fatalf("verify after resume: n=%d err=%v", n, err)
	}
}

func TestTamperDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, _ := Open(path, 0)
	for i := 0; i < 5; i++ {
		l.Log("in", "S1", []byte("8=FIX.4.4\x0135=D\x0111=ord"+string(rune('a'+i))+"\x01"))
	}
	l.Close()

	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")

	edited := append([]string{}, lines...)
	edited[2] = strings.Replace(edited[2], "ordc", "ordX", 1)
	if edited[2] == lines[2] {
		t.Fatal("test bug: edit did not change the line")
	}
	os.WriteFile(path, []byte(strings.Join(edited, "\n")+"\n"), 0o640)
	if _, err := Verify(path); err == nil {
		t.Error("edited line not detected")
	}

	deleted := append(append([]string{}, lines[:2]...), lines[3:]...)
	os.WriteFile(path, []byte(strings.Join(deleted, "\n")+"\n"), 0o640)
	if _, err := Verify(path); err == nil {
		t.Error("deleted line not detected")
	}
}
