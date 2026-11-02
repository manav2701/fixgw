// Package audit writes an append-only, hash-chained record of every FIX message
// in and out of the gateway.
//
// Each line is one JSON object. Entries carry a sequence number and the SHA-256
// of the previous entry, so deleting or editing a line is detectable with Verify.
// Secrets (Password, RawData) are redacted before anything is written.
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"
)

// Entry is one audit record.
type Entry struct {
	Seq     uint64 `json:"seq"`
	TsNs    int64  `json:"ts_ns"`
	Dir     string `json:"dir"` // "in" or "out"
	Session string `json:"session"`
	Msg     string `json:"msg"` // SOH replaced by '|'
	Prev    string `json:"prev"`
	Hash    string `json:"hash"`
}

func (e Entry) digest() string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d|%d|%s|%s|%s|%s", e.Seq, e.TsNs, e.Dir, e.Session, e.Msg, e.Prev)))
	return hex.EncodeToString(h[:])
}

const genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// Logger appends entries to a file. It is safe for concurrent use.
type Logger struct {
	mu   sync.Mutex
	f    *os.File
	w    *bufio.Writer
	seq  uint64
	prev string

	syncEvery time.Duration
	closeOnce sync.Once
	stop      chan struct{}
	done      chan struct{}
	now       func() time.Time
}

// Open opens (or creates) the audit file and resumes the hash chain. With
// syncEvery > 0 the file is flushed and fsynced on that interval; with 0 every
// entry is fsynced before Log returns.
func Open(path string, syncEvery time.Duration) (*Logger, error) {
	seq, prev, err := lastEntry(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	l := &Logger{
		f: f, w: bufio.NewWriterSize(f, 64<<10), seq: seq, prev: prev,
		syncEvery: syncEvery, stop: make(chan struct{}), done: make(chan struct{}),
		now: time.Now,
	}
	if syncEvery > 0 {
		go l.syncLoop()
	} else {
		close(l.done)
	}
	return l, nil
}

func (l *Logger) syncLoop() {
	defer close(l.done)
	t := time.NewTicker(l.syncEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.mu.Lock()
			l.flushLocked()
			l.mu.Unlock()
		case <-l.stop:
			return
		}
	}
}

func (l *Logger) flushLocked() {
	_ = l.w.Flush()
	_ = l.f.Sync()
}

var redactRe = regexp.MustCompile(`(^|\x01)(554|96)=[^\x01]*`)

// Redact masks Password(554) and RawData(96) values in a raw FIX message.
func Redact(raw []byte) []byte {
	if !bytes.Contains(raw, []byte("\x01554=")) && !bytes.Contains(raw, []byte("\x0196=")) {
		return raw
	}
	return redactRe.ReplaceAll(raw, []byte("${1}${2}=***"))
}

// Log records one raw FIX message. dir is "in" or "out".
func (l *Logger) Log(dir, session string, raw []byte) {
	msg := bytes.ReplaceAll(Redact(raw), []byte{0x01}, []byte{'|'})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e := Entry{Seq: l.seq, TsNs: l.now().UnixNano(), Dir: dir, Session: session, Msg: string(msg), Prev: l.prev}
	e.Hash = e.digest()
	line, _ := json.Marshal(e)
	l.w.Write(line)
	l.w.WriteByte('\n')
	l.prev = e.Hash
	if l.syncEvery <= 0 {
		l.flushLocked()
	}
}

// Close flushes, fsyncs and closes the file.
func (l *Logger) Close() error {
	var err error
	l.closeOnce.Do(func() {
		if l.syncEvery > 0 {
			close(l.stop)
		}
		<-l.done
		l.mu.Lock()
		defer l.mu.Unlock()
		l.flushLocked()
		err = l.f.Close()
	})
	return err
}

func lastEntry(path string) (uint64, string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return 0, genesis, nil
	}
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	var last Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if err := json.Unmarshal(sc.Bytes(), &last); err != nil {
			return 0, "", fmt.Errorf("audit: corrupt tail in %s: %w", path, err)
		}
	}
	if err := sc.Err(); err != nil {
		return 0, "", err
	}
	if last.Hash == "" {
		return 0, genesis, nil
	}
	return last.Seq, last.Hash, nil
}

// Verify checks sequence continuity and the hash chain of an audit file and
// returns the number of entries.
func Verify(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	prev := genesis
	var n int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return n, fmt.Errorf("audit: line %d: %w", n+1, err)
		}
		if e.Seq != uint64(n+1) {
			return n, fmt.Errorf("audit: line %d: sequence %d, want %d", n+1, e.Seq, n+1)
		}
		if e.Prev != prev {
			return n, fmt.Errorf("audit: line %d: chain broken", n+1)
		}
		if e.Hash != e.digest() {
			return n, fmt.Errorf("audit: line %d: hash mismatch", n+1)
		}
		prev = e.Hash
		n++
	}
	return n, sc.Err()
}
