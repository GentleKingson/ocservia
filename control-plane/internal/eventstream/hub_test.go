package eventstream

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
)

func NewHub(config Config, fetch FetchFunc, resolve ResolveFunc) (*Hub, error) {
	budget, err := NewWatcherBudget(config.Watchers)
	if err != nil {
		return nil, err
	}
	return NewHubWithWatcherBudget(config, budget, fetch, resolve)
}

func rejectNonNilCursor(_ context.Context, _ string, cursor uuid.UUID) (uint64, error) {
	if cursor != uuid.Nil {
		return 0, ErrInvalidCursor
	}
	return 0, nil
}

func TestSubscribersShareOnePollingWatcher(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		config := DefaultConfig()
		config.PollInterval = 100 * time.Millisecond
		var queries atomic.Uint64
		hub, err := NewHub(config, func(context.Context, string, uuid.UUID, int) ([]Event, error) {
			queries.Add(1)
			return nil, nil
		}, rejectNonNilCursor)
		testutil.NoError(t, err)
		defer hub.Close()

		var subs []*Subscription
		for range 8 {
			sub, err := hub.Subscribe(t.Context(), "workspace-a", uuid.Nil)
			testutil.NoError(t, err)
			subs = append(subs, sub)
		}
		if hub.Snapshot().Watchers != 1 {
			t.Fatalf("watchers=%d, want 1", hub.Snapshot().Watchers)
		}
		time.Sleep(350 * time.Millisecond)
		synctest.Wait()
		if count := queries.Load(); count < 2 || count > 5 {
			t.Fatalf("shared poll queries = %d, want 2..5", count)
		}
		for _, sub := range subs {
			sub.Close()
		}
		synctest.Wait()
		if hub.Snapshot().Watchers != 0 {
			t.Fatalf("watchers=%d after close", hub.Snapshot().Watchers)
		}
	})
}

func TestSlowConsumerDisconnectedWithoutBlockingPeer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		config := DefaultConfig()
		config.PollInterval = 100 * time.Millisecond
		config.SubscriberQueue = 8
		var seq atomic.Uint64
		hub, err := NewHub(config, func(context.Context, string, uuid.UUID, int) ([]Event, error) {
			events := make([]Event, 0, 8)
			for range 8 {
				n := seq.Add(1)
				events = append(events, Event{ID: uuid.Must(uuid.NewV7()), Sequence: n, Name: "test", Data: []byte(fmt.Sprintf("%d", n))})
			}
			return events, nil
		}, rejectNonNilCursor)
		testutil.NoError(t, err)
		defer hub.Close()

		slow, err := hub.Subscribe(t.Context(), "scope", uuid.Nil)
		testutil.NoError(t, err)
		defer slow.Close()
		fast, err := hub.Subscribe(t.Context(), "scope", uuid.Nil)
		testutil.NoError(t, err)
		defer fast.Close()

		for range 16 {
			<-fast.Events
		}
		if err := <-slow.Done; !errors.Is(err, ErrSlowConsumer) {
			t.Fatalf("slow consumer error = %v", err)
		}
	})
}

func TestDatabaseFailureBacksOffAndCursorRecovers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		config := DefaultConfig()
		config.PollInterval = 100 * time.Millisecond
		config.DatabaseMaxBackoff = 400 * time.Millisecond
		var fail atomic.Bool
		var eventID atomic.Value
		fail.Store(true)
		hub, err := NewHub(config, func(_ context.Context, _ string, after uuid.UUID, _ int) ([]Event, error) {
			if fail.Load() {
				return nil, errors.New("database paused")
			}
			id, _ := eventID.Load().(uuid.UUID)
			if id == uuid.Nil || id == after {
				return nil, nil
			}
			return []Event{{ID: id, Sequence: 1, Name: "test", Data: []byte("recovered")}}, nil
		}, rejectNonNilCursor)
		testutil.NoError(t, err)
		defer hub.Close()

		sub, err := hub.Subscribe(t.Context(), "scope", uuid.Nil)
		testutil.NoError(t, err)
		defer sub.Close()
		time.Sleep(150 * time.Millisecond)
		synctest.Wait()
		if _, err := hub.Subscribe(t.Context(), "scope", uuid.Nil); !errors.Is(err, ErrDatabaseBackoff) {
			t.Fatalf("expected ErrDatabaseBackoff, got %v", err)
		}
		id := uuid.Must(uuid.NewV7())
		eventID.Store(id)
		fail.Store(false)
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if ev := <-sub.Events; ev.ID != id {
			t.Fatalf("recovered event = %s, want %s", ev.ID, id)
		}
	})
}

func TestLaggingReconnectDoesNotRewindOrStallLiveSubscribers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		config := DefaultConfig()
		config.PollInterval = 100 * time.Millisecond
		config.SubscriberQueue = 32
		var mu sync.Mutex
		var events []Event
		appendEvents := func(n int) {
			mu.Lock()
			defer mu.Unlock()
			for range n {
				seq := uint64(len(events) + 1)
				events = append(events, Event{ID: uuid.Must(uuid.NewV7()), Sequence: seq, Name: "platform"})
			}
		}
		appendEvents(3)
		fetch := func(_ context.Context, _ string, after uuid.UUID, limit int) ([]Event, error) {
			mu.Lock()
			defer mu.Unlock()
			var out []Event
			seen := after == uuid.Nil
			for _, ev := range events {
				if seen {
					out = append(out, ev)
					if len(out) >= limit {
						break
					}
				}
				if ev.ID == after {
					seen = true
				}
			}
			return out, nil
		}
		resolve := func(_ context.Context, _ string, cursor uuid.UUID) (uint64, error) {
			mu.Lock()
			defer mu.Unlock()
			for _, ev := range events {
				if ev.ID == cursor {
					return ev.Sequence, nil
				}
			}
			return 0, ErrInvalidCursor
		}
		hub, err := NewHub(config, fetch, resolve)
		testutil.NoError(t, err)
		defer hub.Close()

		live, err := hub.Subscribe(t.Context(), "ws", uuid.Nil)
		testutil.NoError(t, err)
		defer live.Close()
		for want := uint64(1); want <= 3; want++ {
			if ev := <-live.Events; ev.Sequence != want {
				t.Fatalf("live seq=%d want %d", ev.Sequence, want)
			}
		}
		mu.Lock()
		firstID := events[0].ID
		mu.Unlock()
		lagging, err := hub.Subscribe(t.Context(), "ws", firstID)
		testutil.NoError(t, err)
		defer lagging.Close()
		appendEvents(2)
		for want := uint64(2); want <= 5; want++ {
			if ev := <-lagging.Events; ev.Sequence != want {
				t.Fatalf("lagging seq=%d want %d", ev.Sequence, want)
			}
		}
	})
}
