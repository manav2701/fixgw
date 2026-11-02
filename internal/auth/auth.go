// Package auth implements the HMAC-signed FIX logon used by the gateway.
//
// The client puts Username(553) and Password(554) in the Logon and a hex
// HMAC-SHA256 signature in RawData(96) over
//
//	SendingTime | MsgType | MsgSeqNum | SenderCompID | TargetCompID | Password
//
// joined by SOH. The same shape is used by several large crypto venues.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Credential is what the gateway knows about one counterparty.
type Credential struct {
	Username string `json:"username"`
	Secret   string `json:"secret"`
	Password string `json:"password"`
}

// Store maps a counterparty CompID (its SenderCompID on inbound messages) to
// its credential.
type Store map[string]Credential

// LoadFile reads a JSON credentials file.
func LoadFile(path string) (Store, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Store
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("auth: parse %s: %w", path, err)
	}
	for id, c := range s {
		if c.Username == "" || c.Secret == "" || c.Password == "" {
			return nil, fmt.Errorf("auth: credential for %q is incomplete", id)
		}
	}
	return s, nil
}

const sendingTimeLayout = "20060102-15:04:05"

// Payload is the string that gets signed.
func Payload(sendingTime, msgType string, seqNum int, senderCompID, targetCompID, password string) string {
	return strings.Join([]string{
		sendingTime, msgType, strconv.Itoa(seqNum), senderCompID, targetCompID, password,
	}, "\x01")
}

// Sign returns the hex HMAC-SHA256 of payload under secret.
func Sign(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// LogonFields are the values lifted from an inbound Logon message.
type LogonFields struct {
	SenderCompID string
	TargetCompID string
	SendingTime  string
	SeqNum       int
	Username     string
	Password     string
	Signature    string
}

var (
	ErrUnknownCounterparty = errors.New("auth: unknown counterparty")
	ErrMalformed           = errors.New("auth: malformed logon")
	ErrStaleLogon          = errors.New("auth: SendingTime outside allowed skew")
	ErrBadCredentials      = errors.New("auth: bad credentials or signature")
	ErrReplay              = errors.New("auth: replayed signature")
)

// Authenticator verifies logons. It is safe for concurrent use.
type Authenticator struct {
	creds   Store
	maxSkew time.Duration

	mu        sync.Mutex
	seen      map[string]time.Time
	lastPurge time.Time
}

// NewAuthenticator returns an Authenticator. maxSkew bounds how far SendingTime
// may differ from the gateway clock, and how long signatures are remembered
// for replay detection.
func NewAuthenticator(creds Store, maxSkew time.Duration) *Authenticator {
	return &Authenticator{creds: creds, maxSkew: maxSkew, seen: make(map[string]time.Time)}
}

// Verify checks a Logon. Callers should return one generic message to the
// client whatever the error, and log the specific error locally.
func (a *Authenticator) Verify(f LogonFields, now time.Time) error {
	cred, ok := a.creds[f.SenderCompID]
	if !ok {
		return ErrUnknownCounterparty
	}
	if f.SendingTime == "" || f.Signature == "" {
		return ErrMalformed
	}
	sent, err := time.Parse(sendingTimeLayout, f.SendingTime)
	if err != nil {
		return ErrMalformed
	}
	if d := now.Sub(sent); d > a.maxSkew || d < -a.maxSkew {
		return ErrStaleLogon
	}

	want := Sign(cred.Secret, Payload(f.SendingTime, "A", f.SeqNum, f.SenderCompID, f.TargetCompID, cred.Password))
	// Evaluate every comparison before branching so timing does not reveal
	// which one failed.
	okUser := eq(f.Username, cred.Username)
	okPass := eq(f.Password, cred.Password)
	okSig := eq(strings.ToLower(f.Signature), want)
	if !(okUser && okPass && okSig) {
		return ErrBadCredentials
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if now.Sub(a.lastPurge) > a.maxSkew {
		for sig, at := range a.seen {
			if now.Sub(at) > 2*a.maxSkew {
				delete(a.seen, sig)
			}
		}
		a.lastPurge = now
	}
	if _, dup := a.seen[want]; dup {
		return ErrReplay
	}
	a.seen[want] = now
	return nil
}

func eq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
