package fixsession

import (
	"path/filepath"
	"testing"

	"github.com/quickfixgo/quickfix"
)

var testSID = quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: "FIXGW", TargetCompID: "CLIENT1"}

func TestSQLiteStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fix.db")

	f, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.Create(testSID)
	if err != nil {
		t.Fatal(err)
	}
	if st.NextSenderMsgSeqNum() != 1 || st.NextTargetMsgSeqNum() != 1 {
		t.Fatalf("fresh store should start at 1/1, got %d/%d", st.NextSenderMsgSeqNum(), st.NextTargetMsgSeqNum())
	}
	for i, body := range []string{"m1", "m2", "m3"} {
		if err := st.SaveMessageAndIncrNextSenderMsgSeqNum(i+1, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	st.IncrNextTargetMsgSeqNum()
	st.IncrNextTargetMsgSeqNum()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// "Restart": a new factory over the same file.
	f, err = OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err = f.Create(testSID)
	if err != nil {
		t.Fatal(err)
	}
	if st.NextSenderMsgSeqNum() != 4 || st.NextTargetMsgSeqNum() != 3 {
		t.Errorf("after restart got %d/%d, want 4/3", st.NextSenderMsgSeqNum(), st.NextTargetMsgSeqNum())
	}
	msgs, err := st.GetMessages(2, 3)
	if err != nil || len(msgs) != 2 || string(msgs[0]) != "m2" || string(msgs[1]) != "m3" {
		t.Errorf("GetMessages(2,3) = %q, %v", msgs, err)
	}

	if err := st.Reset(); err != nil {
		t.Fatal(err)
	}
	if st.NextSenderMsgSeqNum() != 1 || st.NextTargetMsgSeqNum() != 1 {
		t.Error("Reset must restart both sequences at 1")
	}
	if msgs, _ := st.GetMessages(1, 10); len(msgs) != 0 {
		t.Errorf("Reset must clear the archive, got %d messages", len(msgs))
	}
}

func TestSQLiteStoreSessionsAreIsolated(t *testing.T) {
	f, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "fix.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	a, _ := f.Create(testSID)
	other := testSID
	other.TargetCompID = "CLIENT2"
	b, _ := f.Create(other)

	a.SetNextSenderMsgSeqNum(42)
	a.SaveMessage(41, []byte("only-a"))
	if b.NextSenderMsgSeqNum() != 1 {
		t.Error("sequence leaked across sessions")
	}
	if msgs, _ := b.GetMessages(1, 100); len(msgs) != 0 {
		t.Error("archive leaked across sessions")
	}
}

func TestIterateMessagesCallbackMayUseStore(t *testing.T) {
	f, err := OpenSQLiteStore(filepath.Join(t.TempDir(), "fix.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Create(testSID)
	st.SaveMessage(1, []byte("a"))
	st.SaveMessage(2, []byte("b"))
	err = st.IterateMessages(1, 2, func([]byte) error {
		return st.SetNextSenderMsgSeqNum(10) // would deadlock if rows were still open
	})
	if err != nil {
		t.Fatal(err)
	}
}
