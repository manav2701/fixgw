package bridge

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func decode(t *testing.T, b []byte) Event {
	t.Helper()
	var e Event
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestHubDropsOldestForSlowSubscriber(t *testing.T) {
	h := NewHub()
	slow := h.Subscribe(4)
	defer slow.Close()

	for i := 0; i < 10; i++ {
		h.Publish("n", i)
	}
	if slow.Dropped() != 6 {
		t.Errorf("dropped = %d, want 6", slow.Dropped())
	}
	// The newest four survive, in order.
	var got []float64
	for i := 0; i < 4; i++ {
		got = append(got, decode(t, <-slow.C()).Data.(float64))
	}
	for i, want := range []float64{6, 7, 8, 9} {
		if got[i] != want {
			t.Fatalf("kept %v, want [6 7 8 9]", got)
		}
	}
	if _, dropped := h.Stats(); dropped != 6 {
		t.Errorf("hub dropped = %d", dropped)
	}
}

func TestHubSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	h := NewHub()
	slow, fast := h.Subscribe(1), h.Subscribe(100)
	defer slow.Close()
	defer fast.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			h.Publish("n", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher blocked behind a slow subscriber")
	}
	if len(fast.C()) != 50 {
		t.Errorf("fast subscriber got %d events, want 50", len(fast.C()))
	}
}

func TestHubSequenceIsMonotonic(t *testing.T) {
	h := NewHub()
	s := h.Subscribe(10)
	defer s.Close()
	for i := 0; i < 5; i++ {
		h.Publish("n", nil)
	}
	var prev uint64
	for i := 0; i < 5; i++ {
		e := decode(t, <-s.C())
		if e.Seq <= prev {
			t.Fatalf("seq %d after %d", e.Seq, prev)
		}
		prev = e.Seq
	}
}

func TestNilHubIsSafe(t *testing.T) {
	var h *Hub
	h.Publish("x", 1)
	if n, d := h.Stats(); n != 0 || d != 0 {
		t.Error("nil hub stats")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	h := NewHub()
	s := h.Subscribe(1)
	s.Close()
	s.Close()
	h.Publish("x", 1) // must not panic on the closed subscriber
}

type fakeSource struct{}

func (fakeSource) Sessions() any     { return []string{"S1"} }
func (fakeSource) OpenOrders() any   { return []string{} }
func (fakeSource) Symbols() []string { return []string{"BTC/USD"} }
func (fakeSource) Halted() bool      { return true }
func (fakeSource) Book(sym string, depth int) (any, bool, error) {
	return map[string]any{"symbol": sym}, true, nil
}

func TestWebSocketSnapshotThenLiveEvents(t *testing.T) {
	hub := NewHub()
	srv := httptest.NewServer(Handler(hub, fakeSource{}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()

	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := decode(t, b)
	if first.Kind != "snapshot" {
		t.Fatalf("first message kind = %q, want snapshot", first.Kind)
	}
	snap := first.Data.(map[string]any)
	if snap["halted"] != true || snap["books"].(map[string]any)["BTC/USD"] == nil {
		t.Errorf("snapshot = %v", snap)
	}

	// The handler subscribes before it writes the snapshot, so publishing once the
	// snapshot has arrived is guaranteed to be seen.
	hub.Publish(KindExec, map[string]string{"cl_ord_id": "A1"})
	_, b, err = c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if e := decode(t, b); e.Kind != KindExec || e.Data.(map[string]any)["cl_ord_id"] != "A1" {
		t.Errorf("live event = %+v", e)
	}
}

func TestWebSocketDisconnectUnsubscribes(t *testing.T) {
	hub := NewHub()
	srv := httptest.NewServer(Handler(hub, fakeSource{}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Read(ctx) // snapshot
	if n, _ := hub.Stats(); n != 1 {
		t.Fatalf("subscribers = %d, want 1", n)
	}
	c.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := hub.Stats(); n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("subscriber leaked after the client disconnected")
}

func TestRunBooksPublishesOnlyChanges(t *testing.T) {
	hub := NewHub()
	sub := hub.Subscribe(16)
	defer sub.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { RunBooks(ctx, hub, fakeSource{}, 5*time.Millisecond); close(done) }()

	time.Sleep(100 * time.Millisecond) // ~20 ticks of an unchanging book
	cancel()
	<-done
	if n := len(sub.C()); n != 1 {
		t.Errorf("published %d book events for an unchanged book, want 1", n)
	}
}
