package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// Source is the read-only view of the gateway the bridge needs. It keeps this
// package independent of the FIX session layer.
type Source interface {
	Sessions() any
	OpenOrders() any
	Symbols() []string
	Book(symbol string, depth int) (snapshot any, ok bool, err error)
	Halted() bool
}

// Snapshot is the first message on a new connection: the full current state, so
// a browser opened mid-session starts consistent and then follows the feed.
type Snapshot struct {
	Sessions any            `json:"sessions"`
	Orders   any            `json:"orders"`
	Books    map[string]any `json:"books"`
	Halted   bool           `json:"halted"`
}

const (
	queueSize    = 512
	writeTimeout = 5 * time.Second
	bookDepth    = 12
)

// Handler serves the WebSocket feed. Same-origin only: the library rejects
// cross-origin upgrades unless told otherwise, and we do not.
func Handler(hub *Hub, src Source) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := c.CloseRead(r.Context()) // we never read client data; this also detects disconnects

		// Subscribe before taking the snapshot so nothing falls in the gap. A
		// client can see an event that the snapshot already reflects; seq lets it
		// tell.
		sub := hub.Subscribe(queueSize)
		defer sub.Close()

		snap := Snapshot{Sessions: src.Sessions(), Orders: src.OpenOrders(), Halted: src.Halted(), Books: map[string]any{}}
		for _, sym := range src.Symbols() {
			if b, ok, err := src.Book(sym, bookDepth); err == nil && ok {
				snap.Books[sym] = b
			}
		}
		if err := write(ctx, c, mustJSON(Event{Time: time.Now().UTC(), Kind: "snapshot", Data: snap})); err != nil {
			return
		}
		for {
			select {
			case b, ok := <-sub.C():
				if !ok {
					return
				}
				if err := write(ctx, c, b); err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	})
}

func write(ctx context.Context, c *websocket.Conn, b []byte) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, b)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// RunBooks publishes a book snapshot for each symbol whenever it changes, at
// most once per interval. It returns when ctx is done.
func RunBooks(ctx context.Context, hub *Hub, src Source, interval time.Duration) {
	last := make(map[string][]byte)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, sym := range src.Symbols() {
				b, ok, err := src.Book(sym, bookDepth)
				if err != nil || !ok {
					continue
				}
				enc := mustJSON(b)
				if bytes.Equal(enc, last[sym]) {
					continue
				}
				last[sym] = enc
				hub.Publish(KindBook, map[string]any{"symbol": sym, "book": json.RawMessage(enc)})
			}
		}
	}
}
