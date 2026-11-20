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

func cloneEvent(event Event) Event {
	cloned := event
	cloned.Data = append([]byte(nil), event.Data...)
	return cloned
}

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

// New subscriptions must not postpone polling for existing subscribers.
func TestPollDuringSubscriptionChurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config := DefaultConfig()
		var queries atomic.Uint64
		hub, err := NewHub(config, func(context.Context, string, uuid.UUID, int) ([]Event, error) {
			queries.Add(1)
			return nil, nil
		}, rejectNonNilCursor)
		testutil.NoError(t, err)
		defer hub.Close()
		for range 4 {
			_, err := hub.Subscribe(t.Context(), "scope", uuid.Nil)
			testutil.NoError(t, err)
			time.Sleep(config.PollInterval / 2)
		}
		synctest.Wait()
		if queries.Load() == 0 {
			t.Fatal("new subscriptions postponed the poll")
		}
	})
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

func TestLaggingReconnectDoesNotRewindOrStallLiveSubscribers(t *testing.T) {
	config := DefaultConfig()
	config.PollInterval = 100 * time.Millisecond
	config.SubscriberQueue = 64

	var mu sync.Mutex
	var events []Event
	var fetchAfter []uuid.UUID
	appendEvents := func(count int) {
		mu.Lock()
		defer mu.Unlock()
		for range count {
			seq := uint64(len(events) + 1)
			events = append(events, Event{
				ID:       uuid.Must(uuid.NewV7()),
				Sequence: seq,
				Name:     "platform",
				Data:     []byte(fmt.Sprintf(`{"seq":%d}`, seq)),
			})
		}
	}
	appendEvents(5)

	fetch := func(_ context.Context, _ string, after uuid.UUID, limit int) ([]Event, error) {
		mu.Lock()
		defer mu.Unlock()
		fetchAfter = append(fetchAfter, after)
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

	live, err := hub.Subscribe(context.Background(), "workspace-live", uuid.Nil)
	testutil.NoError(t, err)
	defer live.Close()

	for wantSeq := uint64(1); wantSeq <= 5; wantSeq++ {
		select {
		case ev := <-live.Events:
			if ev.Sequence != wantSeq {
				t.Fatalf("live initial seq = %d, want %d", ev.Sequence, wantSeq)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for live seq %d", wantSeq)
		}
	}

	mu.Lock()
	firstEventID := events[0].ID
	fifthEventID := events[4].ID
	mu.Unlock()

	// Reconnect a lagging subscriber at event 1 while new live events 6..10 arrive.
	lagging, err := hub.Subscribe(context.Background(), "workspace-live", firstEventID)
	testutil.NoError(t, err)
	defer lagging.Close()
	appendEvents(5)

	// Live subscriber must receive strictly 6..10 with no rewind or duplicates.
	for wantSeq := uint64(6); wantSeq <= 10; wantSeq++ {
		select {
		case ev := <-live.Events:
			if ev.Sequence != wantSeq {
				t.Fatalf("live post-reconnect seq = %d, want %d", ev.Sequence, wantSeq)
			}
		case <-time.After(time.Second):
			t.Fatalf("live subscriber stalled waiting for seq %d", wantSeq)
		}
	}

	// Lagging subscriber must catch up through 2..5 and hand off cleanly to 6..10.
	for wantSeq := uint64(2); wantSeq <= 10; wantSeq++ {
		select {
		case ev := <-lagging.Events:
			if ev.Sequence != wantSeq {
				t.Fatalf("lagging seq = %d, want %d", ev.Sequence, wantSeq)
			}
		case <-time.After(time.Second):
			t.Fatalf("lagging subscriber stalled waiting for seq %d", wantSeq)
		}
	}

	// Confirm the shared live poll continued forward from event 5 (not rewound to 1).
	mu.Lock()
	sawLiveContinueFromFive := false
	for _, cursor := range fetchAfter {
		if cursor == fifthEventID {
			sawLiveContinueFromFive = true
			break
		}
	}
	mu.Unlock()
	if !sawLiveContinueFromFive {
		t.Fatal("shared live watcher did not continue advancing from seq 5 after lagging reconnect")
	}
}
