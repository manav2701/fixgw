// Package bridge fans gateway events out to browsers over WebSocket.
//
// Publishers (the FIX session layer, the venue router) never block on a slow
// consumer: every subscriber has a bounded queue, and when it is full the oldest
// event is dropped and counted. A stalled dashboard therefore costs the gateway
// nothing.
package bridge

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// Event kinds.
const (
	KindExec    = "exec"    // ExecutionReport sent to a client
	KindCxlRej  = "cxlrej"  // OrderCancelReject sent to a client
	KindWire    = "fix"     // raw FIX message, secrets redacted
	KindSession = "session" // logon/logout
	KindBook    = "book"    // order book snapshot
	KindVenue   = "venue"   // venue routing and reconciliation events
	KindHalt    = "halt"    // kill switch changed
)

// Event is one message on the feed.
type Event struct {
	Seq  uint64    `json:"seq"`
	Time time.Time `json:"time"`
	Kind string    `json:"kind"`
	Data any       `json:"data"`
}

// Hub is a publish/subscribe fan-out with drop-oldest back-pressure. A nil
// *Hub is valid and discards everything, so publishers need no nil checks.
type Hub struct {
	seq     atomic.Uint64
	dropped atomic.Uint64

	mu   sync.RWMutex
	subs map[*Sub]struct{}
}

func NewHub() *Hub { return &Hub{subs: make(map[*Sub]struct{})} }

// Sub is one subscriber's queue.
type Sub struct {
	hub     *Hub
	ch      chan []byte
	dropped atomic.Uint64
}

// C delivers encoded events. It is closed by Close.
func (s *Sub) C() <-chan []byte { return s.ch }

// Dropped is how many events were discarded because this subscriber was slow.
func (s *Sub) Dropped() uint64 { return s.dropped.Load() }

// Close unsubscribes.
func (s *Sub) Close() {
	s.hub.mu.Lock()
	if _, ok := s.hub.subs[s]; ok {
		delete(s.hub.subs, s)
		close(s.ch)
	}
	s.hub.mu.Unlock()
}

// Subscribe registers a subscriber with a queue of size events.
func (h *Hub) Subscribe(size int) *Sub {
	if size < 1 {
		size = 256
	}
	s := &Sub{hub: h, ch: make(chan []byte, size)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Publish stamps and delivers an event to every subscriber.
func (h *Hub) Publish(kind string, data any) {
	if h == nil {
		return
	}
	b, err := json.Marshal(Event{Seq: h.seq.Add(1), Time: time.Now().UTC(), Kind: kind, Data: data})
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		for {
			select {
			case s.ch <- b:
			default:
				// Full: discard the oldest and retry. Publish holds the read lock,
				// so Close cannot close the channel underneath us.
				select {
				case <-s.ch:
					s.dropped.Add(1)
					h.dropped.Add(1)
				default:
				}
				continue
			}
			break
		}
	}
}

// Stats reports subscriber count and total drops.
func (h *Hub) Stats() (subscribers int, dropped uint64) {
	if h == nil {
		return 0, 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs), h.dropped.Load()
}
