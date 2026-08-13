package eventstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func rejectNonNilCursor(_ context.Context, _ string, cursor uuid.UUID) (uint64, error) {
	if cursor != uuid.Nil {
		return 0, ErrInvalidCursor
	}
	return 0, nil
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
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	subscriptions := make([]*Subscription, 0, 100)
	for index := 0; index < 100; index++ {
		subscription, err := hub.Subscribe(context.Background(), "workspace-a", uuid.Nil)
		if err != nil {
			t.Fatalf("subscribe %d: %v", index, err)
		}
		subscriptions = append(subscriptions, subscription)
	}
	if queries.Load() != 0 {
		t.Fatal("subscriber admission performed a subscriber-owned database query")
	}
	if snapshot := hub.Snapshot(); snapshot.Watchers != 1 {
		t.Fatalf("watchers = %d, want 1", snapshot.Watchers)
	}
	time.Sleep(350 * time.Millisecond)
	pollQueries := queries.Load()
	if pollQueries < 2 || pollQueries > 5 {
		t.Fatalf("shared steady-state poll queries = %d, want bounded single-watcher polling", pollQueries)
	}
	for _, subscription := range subscriptions {
		subscription.Close()
	}
	eventually(t, time.Second, func() bool { return hub.Snapshot().Watchers == 0 })
}

func TestSlowConsumerIsDisconnectedWithoutBlockingPeer(t *testing.T) {
	config := DefaultConfig()
	config.PollInterval = 100 * time.Millisecond
	config.SubscriberQueue = 8
	var sequence atomic.Uint64
	hub, err := NewHub(config, func(_ context.Context, _ string, _ uuid.UUID, _ int) ([]Event, error) {
		events := make([]Event, 0, 8)
		for index := 0; index < 8; index++ {
			id := uuid.Must(uuid.NewV7())
			next := sequence.Add(1)
			events = append(events, Event{ID: id, Sequence: next, Name: "test", Data: []byte(fmt.Sprintf("%d", next))})
		}
		return events, nil
	}, rejectNonNilCursor)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	slow, err := hub.Subscribe(context.Background(), "scope", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	fast, err := hub.Subscribe(context.Background(), "scope", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	// Drain the initial catch-up and all shared events from the fast peer only.
	for index := 0; index < 16; index++ {
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
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	subscription, err := hub.Subscribe(context.Background(), "scope", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	eventually(t, time.Second, func() bool {
		snapshot := hub.Snapshot()
		return snapshot.DatabaseErrors > 0 && snapshot.UnhealthyWatchers == 1
	})
	before := queries.Load()
	if _, err := hub.Subscribe(context.Background(), "scope", uuid.Nil); !errors.Is(err, ErrDatabaseBackoff) {
		t.Fatalf("backoff admission error = %v", err)
	}
	if queries.Load() != before {
		t.Fatal("subscriber caused another query during database backoff")
	}
	time.Sleep(300 * time.Millisecond)
	fail.Store(false)
	id := uuid.Must(uuid.NewV7())
	eventID.Store(id)
	eventually(t, 2*time.Second, func() bool {
		snapshot := hub.Snapshot()
		return snapshot.DatabaseBackoff == 0 && snapshot.UnhealthyWatchers == 0
	})
	select {
	case event := <-subscription.Events:
		if event.ID != id {
			t.Fatalf("recovered event = %s, want %s", event.ID, id)
		}
	case <-time.After(time.Second):
		t.Fatal("durable cursor did not recover after database resumed")
	}
}

func TestWatcherQueryCountIsIndependentOfSubscriberCount(t *testing.T) {
	for _, subscriberCount := range []int{1, 10} {
		t.Run(fmt.Sprintf("subscribers-%d", subscriberCount), func(t *testing.T) {
			config := DefaultConfig()
			config.PollInterval = 100 * time.Millisecond
			var queries atomic.Uint64
			hub, err := NewHub(config, func(context.Context, string, uuid.UUID, int) ([]Event, error) {
				queries.Add(1)
				return nil, nil
			}, rejectNonNilCursor)
			if err != nil {
				t.Fatal(err)
			}
			defer hub.Close()
			var subscriptions []*Subscription
			for index := 0; index < subscriberCount; index++ {
				subscription, err := hub.Subscribe(context.Background(), "scope", uuid.Nil)
				if err != nil {
					t.Fatal(err)
				}
				subscriptions = append(subscriptions, subscription)
			}
			if queries.Load() != 0 {
				t.Fatal("subscriber admission issued a database query")
			}
			time.Sleep(350 * time.Millisecond)
			if count := queries.Load(); count < 2 || count > 5 {
				t.Fatalf("shared query count = %d", count)
			}
			for _, subscription := range subscriptions {
				subscription.Close()
			}
		})
	}
}

func TestTerminalEventDrainsThenClosesAllSubscribers(t *testing.T) {
	config := DefaultConfig()
	config.PollInterval = 100 * time.Millisecond
	firstID, terminalID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	var once atomic.Bool
	hub, err := NewHub(config, func(context.Context, string, uuid.UUID, int) ([]Event, error) {
		if once.Swap(true) {
			return nil, nil
		}
		return []Event{{ID: firstID, Sequence: 1, Name: "operation", Data: []byte("running")}, {ID: terminalID, Sequence: 2, Name: "operation", Data: []byte("succeeded"), Terminal: true}}, nil
	}, rejectNonNilCursor)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	subscription, err := hub.Subscribe(context.Background(), "operation", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	var received []Event
	for event := range subscription.Events {
		received = append(received, event)
	}
	if len(received) != 2 || received[0].ID != firstID || received[1].ID != terminalID {
		t.Fatalf("terminal drain = %+v", received)
	}
	select {
	case _, ok := <-subscription.Done:
		if ok {
			t.Fatal("terminal subscription returned an unexpected error")
		}
	default:
		t.Fatal("terminal subscription did not close its completion channel")
	}
}

func TestWatcherCapacityRejectsRandomScopesAndReclaims(t *testing.T) {
	config := DefaultConfig()
	config.Watchers = 2
	config.PollInterval = 100 * time.Millisecond
	hub, err := NewHub(config, func(context.Context, string, uuid.UUID, int) ([]Event, error) { return nil, nil }, rejectNonNilCursor)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	first, err := hub.Subscribe(context.Background(), "scope-a", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hub.Subscribe(context.Background(), "scope-b", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := hub.Subscribe(context.Background(), "attacker-random-scope", uuid.Nil); !errors.Is(err, ErrWatcherLimit) {
		t.Fatalf("watcher capacity error = %v", err)
	}
	first.Close()
	eventually(t, time.Second, func() bool { return hub.Snapshot().Watchers == 1 })
	reclaimed, err := hub.Subscribe(context.Background(), "scope-c", uuid.Nil)
	if err != nil {
		t.Fatalf("reclaimed watcher admission: %v", err)
	}
	reclaimed.Close()
}

func TestScopedWatchersPollFairly(t *testing.T) {
	config := DefaultConfig()
	config.PollInterval = 100 * time.Millisecond
	var mu sync.Mutex
	queries := map[string]int{}
	hub, err := NewHub(config, func(_ context.Context, scope string, _ uuid.UUID, _ int) ([]Event, error) {
		mu.Lock()
		queries[scope]++
		mu.Unlock()
		return nil, nil
	}, rejectNonNilCursor)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	var subscriptions []*Subscription
	for _, scope := range []string{"workspace-a", "workspace-b", "workspace-c"} {
		subscription, err := hub.Subscribe(context.Background(), scope, uuid.Nil)
		if err != nil {
			t.Fatal(err)
		}
		subscriptions = append(subscriptions, subscription)
	}
	defer func() {
		for _, subscription := range subscriptions {
			subscription.Close()
		}
	}()
	eventually(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return queries["workspace-a"] >= 2 && queries["workspace-b"] >= 2 && queries["workspace-c"] >= 2
	})
}

func TestHubRestartResumesFromDurableCursor(t *testing.T) {
	config := DefaultConfig()
	config.PollInterval = 100 * time.Millisecond
	firstID, secondID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	var mu sync.Mutex
	events := []Event{{ID: firstID, Sequence: 1, Name: "platform", Data: []byte("first")}}
	fetch := func(_ context.Context, _ string, after uuid.UUID, _ int) ([]Event, error) {
		mu.Lock()
		defer mu.Unlock()
		var result []Event
		for _, event := range events {
			if after == uuid.Nil || bytes.Compare(event.ID[:], after[:]) > 0 {
				result = append(result, cloneEvent(event))
			}
		}
		return result, nil
	}
	resolve := func(_ context.Context, _ string, cursor uuid.UUID) (uint64, error) {
		for _, event := range events {
			if event.ID == cursor {
				return event.Sequence, nil
			}
		}
		return 0, ErrInvalidCursor
	}
	firstHub, err := NewHub(config, fetch, resolve)
	if err != nil {
		t.Fatal(err)
	}
	firstSubscription, err := firstHub.Subscribe(context.Background(), "workspace", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-firstSubscription.Events:
		if event.ID != firstID {
			t.Fatalf("first event = %s", event.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("initial durable event was not delivered")
	}
	firstSubscription.Close()
	firstHub.Close()
	mu.Lock()
	events = append(events, Event{ID: secondID, Sequence: 2, Name: "platform", Data: []byte("second")})
	mu.Unlock()
	secondHub, err := NewHub(config, fetch, resolve)
	if err != nil {
		t.Fatal(err)
	}
	defer secondHub.Close()
	secondSubscription, err := secondHub.Subscribe(context.Background(), "workspace", firstID)
	if err != nil {
		t.Fatal(err)
	}
	defer secondSubscription.Close()
	select {
	case event := <-secondSubscription.Events:
		if event.ID != secondID {
			t.Fatalf("event after restart = %s, want %s", event.ID, secondID)
		}
	case <-time.After(time.Second):
		t.Fatal("durable cursor did not resume after hub restart")
	}
}

func TestReconnectReplaysByDurableOrderWhenEventIDsAreNotMonotonic(t *testing.T) {
	config := DefaultConfig()
	config.PollInterval = 100 * time.Millisecond
	firstID := uuid.MustParse("019ff100-0000-7000-8000-ffffffffffff")
	secondID := uuid.MustParse("019ff100-0000-7000-8000-000000000001")
	events := []Event{
		{ID: firstID, Sequence: 1, Name: "platform", Data: []byte("first")},
		{ID: secondID, Sequence: 2, Name: "platform", Data: []byte("second")},
	}
	fetch := func(_ context.Context, _ string, after uuid.UUID, _ int) ([]Event, error) {
		start := 0
		if after != uuid.Nil {
			start = len(events)
			for index, event := range events {
				if event.ID == after {
					start = index + 1
					break
				}
			}
		}
		return append([]Event(nil), events[start:]...), nil
	}
	resolve := func(_ context.Context, _ string, cursor uuid.UUID) (uint64, error) {
		for _, event := range events {
			if event.ID == cursor {
				return event.Sequence, nil
			}
		}
		return 0, ErrInvalidCursor
	}
	hub, err := NewHub(config, fetch, resolve)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()

	first, err := hub.Subscribe(context.Background(), "workspace", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	for _, want := range []uuid.UUID{firstID, secondID} {
		select {
		case event := <-first.Events:
			if event.ID != want {
				t.Fatalf("initial event = %s, want %s", event.ID, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("initial event %s was not delivered", want)
		}
	}

	reconnected, err := hub.Subscribe(context.Background(), "workspace", firstID)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	select {
	case event := <-reconnected.Events:
		if event.ID != secondID {
			t.Fatalf("replayed event = %s, want %s", event.ID, secondID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("shared watcher did not replay an event ordered after Last-Event-ID")
	}
	select {
	case duplicate := <-reconnected.Events:
		t.Fatalf("replayed event was delivered twice: %s", duplicate.ID)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestReconnectCatchupAndLiveHandoffDeliversConcurrentCommitExactlyOnce(t *testing.T) {
	config := DefaultConfig()
	config.PollInterval = 100 * time.Millisecond
	firstID, secondID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	var mu sync.Mutex
	events := []Event{{ID: firstID, Sequence: 1, Name: "platform", Data: []byte("first")}}
	queryStarted := make(chan struct{}, 1)
	releaseQuery := make(chan struct{})
	var block atomic.Bool
	fetch := func(_ context.Context, _ string, after uuid.UUID, _ int) ([]Event, error) {
		mu.Lock()
		var result []Event
		for _, event := range events {
			if after == uuid.Nil || event.ID != after {
				result = append(result, cloneEvent(event))
			}
		}
		mu.Unlock()
		if block.CompareAndSwap(true, false) {
			queryStarted <- struct{}{}
			<-releaseQuery
		}
		return result, nil
	}
	resolve := func(_ context.Context, _ string, cursor uuid.UUID) (uint64, error) {
		if cursor == firstID {
			return 1, nil
		}
		return 0, ErrInvalidCursor
	}
	hub, err := NewHub(config, fetch, resolve)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	initial, err := hub.Subscribe(context.Background(), "workspace", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Close()
	select {
	case <-initial.Events:
	case <-time.After(time.Second):
		t.Fatal("initial event was not delivered")
	}

	block.Store(true)
	reconnected, err := hub.Subscribe(context.Background(), "workspace", firstID)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	select {
	case <-queryStarted:
	case <-time.After(time.Second):
		t.Fatal("catch-up query did not start")
	}
	mu.Lock()
	events = append(events, Event{ID: secondID, Sequence: 2, Name: "platform", Data: []byte("second")})
	mu.Unlock()
	close(releaseQuery)
	select {
	case event := <-reconnected.Events:
		if event.ID != secondID {
			t.Fatalf("handoff event = %s, want %s", event.ID, secondID)
		}
	case <-time.After(time.Second):
		t.Fatal("event committed during catch-up/live handoff was lost")
	}
	select {
	case duplicate := <-reconnected.Events:
		t.Fatalf("handoff event was delivered twice: %s", duplicate.ID)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestReconnectStormUsesSharedWatcherAndCachedCursor(t *testing.T) {
	config := DefaultConfig()
	config.PollInterval = 100 * time.Millisecond
	config.SubscriberQueue = 256
	firstID, secondID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	events := []Event{{ID: firstID, Sequence: 1, Name: "platform"}, {ID: secondID, Sequence: 2, Name: "platform"}}
	var queries, resolutions atomic.Uint64
	fetch := func(_ context.Context, _ string, after uuid.UUID, _ int) ([]Event, error) {
		queries.Add(1)
		start := 0
		for index, event := range events {
			if event.ID == after {
				start = index + 1
			}
		}
		return append([]Event(nil), events[start:]...), nil
	}
	resolve := func(_ context.Context, _ string, cursor uuid.UUID) (uint64, error) {
		resolutions.Add(1)
		if cursor == firstID {
			return 1, nil
		}
		return 0, ErrInvalidCursor
	}
	hub, err := NewHub(config, fetch, resolve)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	initial, err := hub.Subscribe(context.Background(), "workspace", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Close()
	for range 2 {
		select {
		case <-initial.Events:
		case <-time.After(time.Second):
			t.Fatal("initial durable events were not delivered")
		}
	}

	subscriptions := make([]*Subscription, 0, 100)
	for range 100 {
		subscription, err := hub.Subscribe(context.Background(), "workspace", firstID)
		if err != nil {
			t.Fatal(err)
		}
		subscriptions = append(subscriptions, subscription)
	}
	defer func() {
		for _, subscription := range subscriptions {
			subscription.Close()
		}
	}()
	for _, subscription := range subscriptions {
		select {
		case event := <-subscription.Events:
			if event.ID != secondID {
				t.Fatalf("storm replay event = %s, want %s", event.ID, secondID)
			}
		case <-time.After(time.Second):
			t.Fatal("reconnect storm subscriber missed durable replay")
		}
	}
	if got := hub.Snapshot().Watchers; got != 1 {
		t.Fatalf("watchers = %d, want 1", got)
	}
	if got := resolutions.Load(); got != 0 {
		t.Fatalf("cached reconnect cursor issued %d resolver queries, want 0", got)
	}
	if got := queries.Load(); got > 4 {
		t.Fatalf("reconnect storm issued %d shared fetch queries", got)
	}
}

func TestInvalidScopedCursorFailsBeforeWatcherPolling(t *testing.T) {
	config := DefaultConfig()
	var queries atomic.Uint64
	hub, err := NewHub(config, func(context.Context, string, uuid.UUID, int) ([]Event, error) {
		queries.Add(1)
		return nil, nil
	}, func(context.Context, string, uuid.UUID) (uint64, error) {
		return 0, ErrInvalidCursor
	})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	foreign := uuid.Must(uuid.NewV7())
	if _, err := hub.Subscribe(context.Background(), "workspace", foreign); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("foreign cursor error = %v", err)
	}
	if queries.Load() != 0 {
		t.Fatal("invalid cursor started durable polling")
	}
}

func TestReconnectAtLatestAfterIdleCleanupWaitsForNextLiveEvent(t *testing.T) {
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
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	initial, err := hub.Subscribe(context.Background(), "workspace", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-initial.Events:
	case <-time.After(time.Second):
		t.Fatal("initial event was not delivered")
	}
	initial.Close()
	eventually(t, time.Second, func() bool { return hub.Snapshot().Watchers == 0 })

	reconnected, err := hub.Subscribe(context.Background(), "workspace", firstID)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	select {
	case duplicate := <-reconnected.Events:
		t.Fatalf("latest cursor replayed historical event %s", duplicate.ID)
	case <-time.After(250 * time.Millisecond):
	}
	mu.Lock()
	events = append(events, Event{ID: secondID, Sequence: 2, Name: "platform"})
	mu.Unlock()
	select {
	case event := <-reconnected.Events:
		if event.ID != secondID {
			t.Fatalf("live event = %s, want %s", event.ID, secondID)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnected subscriber did not receive the next live event")
	}
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
