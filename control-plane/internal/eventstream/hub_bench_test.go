package eventstream

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
)

func BenchmarkWatcherFanout(b *testing.B) {
	payload := bytes.Repeat([]byte(`{"type":"heartbeat","node":"n1"}`), 16) // 512B event payload
	events := make([]Event, 100)
	for i := range events {
		events[i] = Event{
			ID:       uuid.Must(uuid.NewV7()),
			Sequence: uint64(i + 1),
			Name:     "platform",
			Data:     payload,
		}
	}

	b.Run("Fanout_64Subscribers_100Events", func(b *testing.B) {
		cfg := DefaultConfig()
		cfg.SubscriberQueue = 256
		hub, err := NewHub(cfg, func(_ context.Context, _ string, _ uuid.UUID, _ int) ([]Event, error) {
			return events, nil
		}, func(_ context.Context, _ string, _ uuid.UUID) (uint64, error) {
			return 0, nil
		})
		if err != nil {
			b.Fatal(err)
		}
		defer hub.Close()

		w := &watcher{
			hub:             hub,
			ctx:             b.Context(),
			scope:           "bench",
			hasCursor:       true,
			subscribers:     make(map[uint64]*subscriber, 64),
			cursorSequences: make(map[uuid.UUID]uint64),
		}
		for i := range uint64(64) {
			w.subscribers[i+1] = &subscriber{
				id:     i + 1,
				events: make(chan Event, 256),
				done:   make(chan error, 1),
			}
		}

		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			w.cursor = uuid.Nil
			w.cursorSequence = 0
			clear(w.cursorSequences)
			w.cursorOrder = w.cursorOrder[:0]
			for _, sub := range w.subscribers {
				sub.sequence = 0
			}
			if _, err := w.poll(); err != nil {
				b.Fatal(err)
			}
			for _, sub := range w.subscribers {
				for len(sub.events) > 0 {
					<-sub.events
				}
			}
		}
	})
}
