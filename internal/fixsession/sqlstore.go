package fixsession

import (
	"database/sql"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"
	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	session_id    TEXT PRIMARY KEY,
	creation_time INTEGER NOT NULL,
	next_sender   INTEGER NOT NULL,
	next_target   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
	session_id TEXT    NOT NULL,
	seq        INTEGER NOT NULL,
	body       BLOB    NOT NULL,
	PRIMARY KEY (session_id, seq)
) WITHOUT ROWID;
`

// SQLiteStoreFactory creates quickfix MessageStores backed by one SQLite
// database. Sequence numbers and the outbound message archive survive process
// restarts, so a restart does not force a sequence reset with every client.
type SQLiteStoreFactory struct {
	db *sql.DB
}

// OpenSQLiteStore opens (creating if needed) the database at path.
func OpenSQLiteStore(path string) (*SQLiteStoreFactory, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite serialises writers anyway
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("fixsession: init schema: %w", err)
	}
	return &SQLiteStoreFactory{db: db}, nil
}

// Close closes the underlying database. Call after the acceptor has stopped.
func (f *SQLiteStoreFactory) Close() error { return f.db.Close() }

// Create implements quickfix.MessageStoreFactory.
func (f *SQLiteStoreFactory) Create(sid quickfix.SessionID) (quickfix.MessageStore, error) {
	s := &sqliteStore{db: f.db, id: sid.String()}
	if err := s.Refresh(); err != nil {
		return nil, err
	}
	return s, nil
}

type sqliteStore struct {
	db *sql.DB
	id string

	mu           sync.Mutex
	nextSender   int
	nextTarget   int
	creationTime time.Time
}

// Refresh loads the session row, creating it on first use.
func (s *sqliteStore) Refresh() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var created int64
	err := s.db.QueryRow(
		`SELECT creation_time, next_sender, next_target FROM sessions WHERE session_id = ?`, s.id,
	).Scan(&created, &s.nextSender, &s.nextTarget)
	if err == sql.ErrNoRows {
		now := time.Now().UTC()
		if _, err := s.db.Exec(
			`INSERT INTO sessions (session_id, creation_time, next_sender, next_target) VALUES (?, ?, 1, 1)`,
			s.id, now.UnixNano(),
		); err != nil {
			return err
		}
		s.creationTime, s.nextSender, s.nextTarget = now, 1, 1
		return nil
	}
	if err != nil {
		return err
	}
	s.creationTime = time.Unix(0, created).UTC()
	return nil
}

func (s *sqliteStore) NextSenderMsgSeqNum() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextSender
}

func (s *sqliteStore) NextTargetMsgSeqNum() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextTarget
}

func (s *sqliteStore) IncrNextSenderMsgSeqNum() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setSender(s.nextSender + 1)
}

func (s *sqliteStore) IncrNextTargetMsgSeqNum() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setTarget(s.nextTarget + 1)
}

func (s *sqliteStore) SetNextSenderMsgSeqNum(next int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setSender(next)
}

func (s *sqliteStore) SetNextTargetMsgSeqNum(next int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setTarget(next)
}

// setSender and setTarget write through to the database before updating the
// cache, so memory never runs ahead of what is durable.
func (s *sqliteStore) setSender(next int) error {
	if _, err := s.db.Exec(`UPDATE sessions SET next_sender = ? WHERE session_id = ?`, next, s.id); err != nil {
		return err
	}
	s.nextSender = next
	return nil
}

func (s *sqliteStore) setTarget(next int) error {
	if _, err := s.db.Exec(`UPDATE sessions SET next_target = ? WHERE session_id = ?`, next, s.id); err != nil {
		return err
	}
	s.nextTarget = next
	return nil
}

func (s *sqliteStore) CreationTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creationTime
}

func (s *sqliteStore) SetCreationTime(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`UPDATE sessions SET creation_time = ? WHERE session_id = ?`, t.UnixNano(), s.id); err == nil {
		s.creationTime = t
	}
}

func (s *sqliteStore) SaveMessage(seq int, msg []byte) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO messages (session_id, seq, body) VALUES (?, ?, ?)`, s.id, seq, msg)
	return err
}

// SaveMessageAndIncrNextSenderMsgSeqNum archives the message and advances the
// sender sequence number in one transaction, so a crash can never leave a
// sent sequence number without its archived copy (which resend needs).
func (s *sqliteStore) SaveMessageAndIncrNextSenderMsgSeqNum(seq int, msg []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR REPLACE INTO messages (session_id, seq, body) VALUES (?, ?, ?)`, s.id, seq, msg); err != nil {
		return err
	}
	next := s.nextSender + 1
	if _, err := tx.Exec(`UPDATE sessions SET next_sender = ? WHERE session_id = ?`, next, s.id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.nextSender = next
	return nil
}

func (s *sqliteStore) IterateMessages(begin, end int, cb func([]byte) error) error {
	rows, err := s.db.Query(
		`SELECT body FROM messages WHERE session_id = ? AND seq BETWEEN ? AND ? ORDER BY seq`, s.id, begin, end)
	if err != nil {
		return err
	}
	// Drain before invoking callbacks: the pool has a single connection and the
	// callback may call back into the store.
	var bodies [][]byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			rows.Close()
			return err
		}
		bodies = append(bodies, b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, b := range bodies {
		if err := cb(b); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqliteStore) GetMessages(begin, end int) ([][]byte, error) {
	var out [][]byte
	err := s.IterateMessages(begin, end, func(b []byte) error {
		out = append(out, b)
		return nil
	})
	return out, err
}

// Reset clears the archive and restarts both sequence numbers at 1.
func (s *sqliteStore) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id = ?`, s.id); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE sessions SET next_sender = 1, next_target = 1, creation_time = ? WHERE session_id = ?`,
		now.UnixNano(), s.id,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.nextSender, s.nextTarget, s.creationTime = 1, 1, now
	return nil
}

// Close is a no-op: the database is owned by the factory.
func (s *sqliteStore) Close() error { return nil }
