package eventstream

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
)

func rejectNonNilCursor(_ context.Context, _ string, cursor uuid.UUID) (uint64, error) {
	if cursor != uuid.Nil {
		return 0, ErrInvalidCursor
	}
	return 0, nil
}

func eventually(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestSubscribersShareOnePollingWatcher(t *testing.T) {
	config := DefaultConfig()
	config.PollInterval = 100 * time.Millisecond
	config.SubscriberQueue = 256
	var queries atomic.Uint64
	hub, err := NewHub(config, func(_ context.Context, _ string, _ uuid.UUID, _ int) ([]Event, error) {
		queries.Add(1)
		return nil, nil
	}, rejectNonNilCursor)
	testutil.NoError(t, err)
	defer hub.Close()

	subs := make([]*Subscription, 0, 32)
	for range 32 {
		sub, err := hub.Subscribe(context.Background(), "workspace-a", uuid.Nil)
		testutil.NoError(t, err)
		subs = append(subs, sub)
	}
	if queries.Load() != 0 || hub.Snapshot().Watchers != 1 {
		t.Fatalf("queries=%d watchers=%d", queries.Load(), hub.Snapshot().Watchers)
	}
	time.Sleep(350 * time.Millisecond)
	if count := queries.Load(); count < 2 || count > 5 {
		t.Fatalf("shared poll queries = %d, want 2..5", count)
	}
	for _, sub := range subs {
		sub.Close()
	}
	eventually(t, time.Second, func() bool { return hub.Snapshot().Watchers == 0 })
}

func TestSlowConsumerDisconnectedWithoutBlockingPeer(t *testing.T) {
	config := DefaultConfig()
	config.PollInterval = 100 * time.Millisecond
	config.SubscriberQueue = 8
	var sequence atomic.Uint64
	hub, err := NewHub(config, func(_ context.Context, _ string, _ uuid.UUID, _ int) ([]Event, error) {
		events := make([]Event, 0, 8)
		for range 8 {
			next := sequence.Add(1)
			events = append(events, Event{ID: uuid.Must(uuid.NewV7()), Sequence: next, Name: "test", Data: []byte(fmt.Sprintf("%d", next))})
		}
		return events, nil
	}, rejectNonNilCursor)
	testutil.NoError(t, err)
	defer hub.Close()

	slow, err := hub.Subscribe(context.Background(), "scope", uuid.Nil)
	testutil.NoError(t, err)
	fast, err := hub.Subscribe(context.Background(), "scope", uuid.Nil)
	testutil.NoError(t, err)
	defer fast.Close()

	for range 16 {
		select {
		case <-fast.Events:
		case <-time.After(time.Second):
			t.Fatal("fast subscriber stopped receiving")
		}
	}
	select {
	case err := <-slow.Done:
		if !errors.Is(err, ErrSlowConsumer) {
			t.Fatalf("slow consumer error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow subscriber was not disconnected")
	}
	if hub.Snapshot().SlowConsumerDisconnects == 0 {
		t.Fatal("slow consumer metric was not incremented")
	}
}

func TestDatabaseFailureBacksOffAndCursorRecovers(t *testing.T) {
	config := DefaultConfig()
	config.PollInterval = 100 * time.Millisecond
	config.DatabaseMaxBackoff = 800 * time.Millisecond
	var queries atomic.Uint64
	var fail atomic.Bool
	var eventID atomic.Value
	fail.Store(true)
	hub, err := NewHub(config, func(_ context.Context, _ string, after uuid.UUID, _ int) ([]Event, error) {
		queries.Add(1)
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

	sub, err := hub.Subscribe(context.Background(), "scope", uuid.Nil)
	testutil.NoError(t, err)
	defer sub.Close()
	eventually(t, time.Second, func() bool {
		s := hub.Snapshot()
		return s.DatabaseErrors > 0 && s.UnhealthyWatchers == 1
	})
	before := queries.Load()
	if _, err := hub.Subscribe(context.Background(), "scope", uuid.Nil); !errors.Is(err, ErrDatabaseBackoff) || queries.Load() != before {
		t.Fatalf("backoff admission err=%v delta=%d", err, queries.Load()-before)
	}
	time.Sleep(300 * time.Millisecond)
	fail.Store(false)
	id := uuid.Must(uuid.NewV7())
	eventID.Store(id)
	eventually(t, 2*time.Second, func() bool {
		s := hub.Snapshot()
		return s.DatabaseBackoff == 0 && s.UnhealthyWatchers == 0
	})
	select {
	case event := <-sub.Events:
		if event.ID != id {
			t.Fatalf("recovered event = %s, want %s", event.ID, id)
		}
	case <-time.After(time.Second):
		t.Fatal("durable cursor did not recover")
	}
}

func TestTerminalEventDrainsAndWatcherCapacityReclaims(t *testing.T) {
	config := DefaultConfig()
	config.PollInterval = 100 * time.Millisecond
	config.Watchers = 2
	firstID, terminalID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	var once atomic.Bool
	hub, err := NewHub(config, func(_ context.Context, scope string, _ uuid.UUID, _ int) ([]Event, error) {
		if scope == "operation" && !once.Swap(true) {
			return []Event{
				{ID: firstID, Sequence: 1, Name: "operation", Data: []byte("running")},
				{ID: terminalID, Sequence: 2, Name: "operation", Data: []byte("succeeded"), Terminal: true},
			}, nil
		}
		return nil, nil
	}, rejectNonNilCursor)
	testutil.NoError(t, err)
	defer hub.Close()

	sub, err := hub.Subscribe(context.Background(), "operation", uuid.Nil)
	testutil.NoError(t, err)
	var received []Event
	for event := range sub.Events {
		received = append(received, event)
	}
	if len(received) != 2 || received[0].ID != firstID || received[1].ID != terminalID {
		t.Fatalf("terminal drain = %+v", received)
	}
	sub.Close()
	eventually(t, time.Second, func() bool { return hub.Snapshot().Watchers == 0 })

	s1, err := hub.Subscribe(context.Background(), "scope-a", uuid.Nil)
	testutil.NoError(t, err)
	s2, err := hub.Subscribe(context.Background(), "scope-b", uuid.Nil)
	testutil.NoError(t, err)
	defer s2.Close()
	if _, err := hub.Subscribe(context.Background(), "scope-overflow", uuid.Nil); !errors.Is(err, ErrWatcherLimit) {
		t.Fatalf("watcher limit error = %v", err)
	}
	s1.Close()
	eventually(t, time.Second, func() bool { return hub.Snapshot().Watchers == 1 })
	s3, err := hub.Subscribe(context.Background(), "scope-c", uuid.Nil)
	testutil.NoError(t, err)
	s3.Close()
}

func TestReconnectCursorValidationAndLiveHandoff(t *testing.T) {
	config := DefaultConfig()
	config.PollInterval = 100 * time.Millisecond
	firstID, secondID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	var mu sync.Mutex
	events := []Event{{ID: firstID, Sequence: 1, Name: "platform"}}
	fetch := func(_ context.Context, _ string, after uuid.UUID, _ int) ([]Event, error) {
		mu.Lock()
		defer mu.Unlock()
		var result []Event
		seen := after == uuid.Nil
		for _, event := range events {
			if seen {
				result = append(result, cloneEvent(event))
			}
			if event.ID == after {
				seen = true
			}
		}
		return result, nil
	}
	resolve := func(_ context.Context, _ string, cursor uuid.UUID) (uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		for _, event := range events {
			if event.ID == cursor {
				return event.Sequence, nil
			}
		}
		return 0, ErrInvalidCursor
	}
	hub, err := NewHub(config, fetch, resolve)
	testutil.NoError(t, err)
	defer hub.Close()

	if _, err := hub.Subscribe(context.Background(), "workspace", uuid.Must(uuid.NewV7())); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("invalid cursor error = %v", err)
	}

	initial, err := hub.Subscribe(context.Background(), "workspace", uuid.Nil)
	testutil.NoError(t, err)
	select {
	case <-initial.Events:
	case <-time.After(time.Second):
		t.Fatal("initial event missing")
	}
	initial.Close()
	eventually(t, time.Second, func() bool { return hub.Snapshot().Watchers == 0 })

	reconnected, err := hub.Subscribe(context.Background(), "workspace", firstID)
	testutil.NoError(t, err)
	defer reconnected.Close()
	select {
	case dup := <-reconnected.Events:
		t.Fatalf("replayed latest cursor event %s", dup.ID)
	case <-time.After(200 * time.Millisecond):
	}
	mu.Lock()
	events = append(events, Event{ID: secondID, Sequence: 2, Name: "platform"})
	mu.Unlock()
	select {
	case ev := <-reconnected.Events:
		if ev.ID != secondID {
			t.Fatalf("live event = %s, want %s", ev.ID, secondID)
		}
	case <-time.After(time.Second):
		t.Fatal("missed next live event after reconnect")
	}
}
