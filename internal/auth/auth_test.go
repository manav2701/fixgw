package auth

import (
	"errors"
	"testing"
	"time"
)

var testCreds = Store{"CLIENT1": {Username: "key1", Secret: "s3cret", Password: "pw"}}

func signedLogon(now time.Time, seq int) LogonFields {
	st := now.UTC().Format(sendingTimeLayout)
	return LogonFields{
		SenderCompID: "CLIENT1", TargetCompID: "FIXGW", SendingTime: st, SeqNum: seq,
		Username: "key1", Password: "pw",
		Signature: Sign("s3cret", Payload(st, "A", seq, "CLIENT1", "FIXGW", "pw")),
	}
}

func TestVerify(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a := NewAuthenticator(testCreds, 2*time.Minute)

	if err := a.Verify(signedLogon(now, 1), now); err != nil {
		t.Fatalf("valid logon rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*LogonFields)
		now    time.Time
		want   error
	}{
		{"unknown counterparty", func(f *LogonFields) { f.SenderCompID = "NOBODY" }, now, ErrUnknownCounterparty},
		{"bad signature", func(f *LogonFields) { f.Signature = "00" }, now, ErrBadCredentials},
		{"bad username", func(f *LogonFields) { f.Username = "other" }, now, ErrBadCredentials},
		{"bad password", func(f *LogonFields) { f.Password = "nope" }, now, ErrBadCredentials},
		{"tampered seqnum", func(f *LogonFields) { f.SeqNum = 99 }, now, ErrBadCredentials},
		{"missing signature", func(f *LogonFields) { f.Signature = "" }, now, ErrMalformed},
		{"garbage time", func(f *LogonFields) { f.SendingTime = "yesterday" }, now, ErrMalformed},
		{"stale", func(f *LogonFields) {}, now.Add(10 * time.Minute), ErrStaleLogon},
		{"from the future", func(f *LogonFields) {}, now.Add(-10 * time.Minute), ErrStaleLogon},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := signedLogon(now, 2)
			tc.mutate(&f)
			if err := a.Verify(f, tc.now); !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReplayRejected(t *testing.T) {
	now := time.Now().UTC()
	a := NewAuthenticator(testCreds, time.Minute)
	f := signedLogon(now, 1)
	if err := a.Verify(f, now); err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(f, now); !errors.Is(err, ErrReplay) {
		t.Errorf("replay: got %v", err)
	}
	// A fresh signature (different seqnum) is fine.
	if err := a.Verify(signedLogon(now, 2), now); err != nil {
		t.Errorf("fresh logon: %v", err)
	}
}

func TestForgedSignatureDoesNotPoisonReplayCache(t *testing.T) {
	now := time.Now().UTC()
	a := NewAuthenticator(testCreds, time.Minute)
	bad := signedLogon(now, 1)
	bad.Username = "wrong"
	if err := a.Verify(bad, now); !errors.Is(err, ErrBadCredentials) {
		t.Fatal(err)
	}
	if err := a.Verify(signedLogon(now, 1), now); err != nil {
		t.Errorf("legitimate logon blocked after forged attempt: %v", err)
	}
}
