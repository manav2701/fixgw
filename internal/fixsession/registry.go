package fixsession

import (
	"sort"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/manav2701/fixgw/internal/fixmsg"
)

// SessionInfo is a point-in-time view of one FIX session, for status APIs.
type SessionInfo struct {
	SessionID    string    `json:"session_id"`
	LoggedOn     bool      `json:"logged_on"`
	InSeq        int       `json:"in_seq"`  // MsgSeqNum of the last inbound message
	OutSeq       int       `json:"out_seq"` // MsgSeqNum of the last outbound message
	MsgsIn       uint64    `json:"msgs_in"`
	MsgsOut      uint64    `json:"msgs_out"`
	Heartbeats   uint64    `json:"heartbeats_in"`
	Orders       uint64    `json:"orders_in"`
	Logons       uint64    `json:"logons"`
	AuthFailures uint64    `json:"auth_failures"`
	LogonAt      time.Time `json:"logon_at,omitempty"`
	LastInAt     time.Time `json:"last_in_at,omitempty"`
	LastOutAt    time.Time `json:"last_out_at,omitempty"`
}

// Registry tracks live state for every session. It is safe for concurrent use.
type Registry struct {
	mu sync.RWMutex
	m  map[string]*SessionInfo
}

func NewRegistry() *Registry { return &Registry{m: make(map[string]*SessionInfo)} }

func (r *Registry) get(id string) *SessionInfo {
	s, ok := r.m[id]
	if !ok {
		s = &SessionInfo{SessionID: id}
		r.m[id] = s
	}
	return s
}

func (r *Registry) Created(id string) {
	r.mu.Lock()
	r.get(id)
	r.mu.Unlock()
}

func (r *Registry) LoggedOn(id string) {
	r.mu.Lock()
	s := r.get(id)
	s.LoggedOn, s.LogonAt = true, time.Now().UTC()
	s.Logons++
	r.mu.Unlock()
}

func (r *Registry) LoggedOut(id string) {
	r.mu.Lock()
	r.get(id).LoggedOn = false
	r.mu.Unlock()
}

func (r *Registry) AuthFailed(id string) {
	r.mu.Lock()
	r.get(id).AuthFailures++
	r.mu.Unlock()
}

// In records an inbound message.
func (r *Registry) In(id string, m *quickfix.Message) {
	seq := fixmsg.Int(&m.Header, fixmsg.TagMsgSeqNum)
	mt := fixmsg.Str(&m.Header, fixmsg.TagMsgType)
	r.mu.Lock()
	s := r.get(id)
	s.MsgsIn++
	s.LastInAt = time.Now().UTC()
	// A PossDup resend carries an old sequence number; do not move backwards.
	if seq > s.InSeq {
		s.InSeq = seq
	}
	switch mt {
	case fixmsg.MsgHeartbeat:
		s.Heartbeats++
	case fixmsg.MsgNewOrderSingle:
		s.Orders++
	}
	r.mu.Unlock()
}

// Out records an outbound message.
func (r *Registry) Out(id string, m *quickfix.Message) {
	seq := fixmsg.Int(&m.Header, fixmsg.TagMsgSeqNum)
	r.mu.Lock()
	s := r.get(id)
	s.MsgsOut++
	s.LastOutAt = time.Now().UTC()
	if seq > s.OutSeq {
		s.OutSeq = seq
	}
	r.mu.Unlock()
}

// Snapshot returns a copy of every session, sorted by ID.
func (r *Registry) Snapshot() []SessionInfo {
	r.mu.RLock()
	out := make([]SessionInfo, 0, len(r.m))
	for _, s := range r.m {
		out = append(out, *s)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}
