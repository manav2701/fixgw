package bridge

import (
	"testing"
)

// BenchmarkPublish is the cost the gateway pays per event for ten connected
// dashboards, all of them draining.
func BenchmarkPublish(b *testing.B) {
	h := NewHub()
	for i := 0; i < 10; i++ {
		s := h.Subscribe(1024)
		go func() {
			for range s.C() {
			}
		}()
		defer s.Close()
	}
	ev := map[string]string{"cl_ord_id": "A1", "symbol": "BTC/USD", "qty": "0.010000"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Publish(KindExec, ev)
	}
}

// BenchmarkPublishNoSubscribers is the cost when nobody is watching.
func BenchmarkPublishNoSubscribers(b *testing.B) {
	h := NewHub()
	ev := map[string]string{"cl_ord_id": "A1"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.Publish(KindExec, ev)
	}
}
