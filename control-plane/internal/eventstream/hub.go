package eventstream

import (
	"context"
	"errors"
	mathrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

var (
	ErrWatcherLimit    = errors.New("event watcher capacity reached")
	ErrDatabaseBackoff = errors.New("event watcher database backoff is active")
	ErrSlowConsumer    = errors.New("event stream subscriber queue overflowed")
	ErrInvalidCursor   = errors.New("event stream cursor is not visible in this scope")
)

const watcherPageSize = 100

type Event struct {
	ID       uuid.UUID
	Sequence uint64
	Name     string
	Data     []byte
	Terminal bool
}

type FetchFunc func(context.Context, string, uuid.UUID, int) ([]Event, error)
type ResolveFunc func(context.Context, string, uuid.UUID) (uint64, error)

type HubSnapshot struct {
	Watchers                int64
	UnhealthyWatchers       int64
	Queries                 uint64
	DatabaseErrors          uint64
	SlowConsumerDisconnects uint64
	DatabaseBackoff         time.Duration
}

type Hub struct {
	ctx      context.Context
	cancel   context.CancelFunc
	config   Config
	budget   *WatcherBudget
	fetch    FetchFunc
	resolve  ResolveFunc
	mu       sync.Mutex
	watchers map[string]*watcher
	closed   bool
	metrics  hubMetrics
}

// WatcherBudget bounds polling watchers across every hub that shares it.
type WatcherBudget struct {
	mu     sync.Mutex
	limit  int
	active int
}

func NewWatcherBudget(limit int) (*WatcherBudget, error) {
	if limit < 1 {
		return nil, errors.New("event watcher capacity must be positive")
	}
	return &WatcherBudget{limit: limit}, nil
}

func (b *WatcherBudget) acquire() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active >= b.limit {
		return false
	}
	b.active++
	return true
}

func (b *WatcherBudget) release() {
	b.mu.Lock()
	b.active--
	b.mu.Unlock()
}

type hubMetrics struct {
	watchers       atomic.Int64
	unhealthy      atomic.Int64
	queries        atomic.Uint64
	databaseErrors atomic.Uint64
	slowConsumers  atomic.Uint64
	backoffNanos   atomic.Int64
}

func NewHub(config Config, fetch FetchFunc, resolve ResolveFunc) (*Hub, error) {
	budget, err := NewWatcherBudget(config.Watchers)
	if err != nil {
		return nil, err
	}
	return NewHubWithWatcherBudget(config, budget, fetch, resolve)
}

func NewHubWithWatcherBudget(config Config, budget *WatcherBudget, fetch FetchFunc, resolve ResolveFunc) (*Hub, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if budget == nil {
		return nil, errors.New("event watcher budget is required")
	}
	if budget.limit != config.Watchers {
		return nil, errors.New("event watcher budget does not match configuration")
	}
	if fetch == nil {
		return nil, errors.New("event source is required")
	}
	if resolve == nil {
		return nil, errors.New("event cursor resolver is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{ctx: ctx, cancel: cancel, config: config, budget: budget, fetch: fetch, resolve: resolve, watchers: make(map[string]*watcher)}, nil
}

func (h *Hub) Subscribe(ctx context.Context, scope string, after uuid.UUID) (*Subscription, error) {
	if scope == "" {
		return nil, errors.New("event watcher scope is required")
	}
	for attempts := 0; attempts < 2; attempts++ {
		w, err := h.watcher(scope)
		if err != nil {
			return nil, err
		}
		request := subscribeRequest{after: after, response: make(chan subscribeResponse, 1)}
		select {
		case w.subscribe <- request:
		case <-w.done:
			continue
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-h.ctx.Done():
			return nil, ErrClosed
		}
		select {
		case response := <-request.response:
			return response.subscription, response.err
		case <-w.done:
			continue
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-h.ctx.Done():
			return nil, ErrClosed
		}
	}
	return nil, ErrClosed
}

func (h *Hub) watcher(scope string) (*watcher, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}
	if existing := h.watchers[scope]; existing != nil {
		return existing, nil
	}
	if !h.budget.acquire() {
		return nil, ErrWatcherLimit
	}
	ctx, cancel := context.WithCancel(h.ctx)
	w := &watcher{
		hub: h, scope: scope, ctx: ctx, cancel: cancel,
		subscribe: make(chan subscribeRequest), unsubscribe: make(chan uint64), done: make(chan struct{}),
		subscribers: make(map[uint64]*subscriber), cursorSequences: make(map[uuid.UUID]uint64),
		backoff: h.config.PollInterval,
	}
	h.watchers[scope] = w
	h.metrics.watchers.Add(1)
	go w.run()
	return w, nil
}

func (h *Hub) remove(scope string, target *watcher) {
	h.mu.Lock()
	if h.watchers[scope] == target {
		delete(h.watchers, scope)
		h.metrics.watchers.Add(-1)
		h.budget.release()
	}
	h.mu.Unlock()
}

func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	h.cancel()
	watchers := make([]*watcher, 0, len(h.watchers))
	for _, w := range h.watchers {
		watchers = append(watchers, w)
	}
	h.mu.Unlock()
	for _, w := range watchers {
		<-w.done
	}
}

func (h *Hub) Snapshot() HubSnapshot {
	return HubSnapshot{
		Watchers: h.metrics.watchers.Load(), Queries: h.metrics.queries.Load(),
		UnhealthyWatchers:       h.metrics.unhealthy.Load(),
		DatabaseErrors:          h.metrics.databaseErrors.Load(),
		SlowConsumerDisconnects: h.metrics.slowConsumers.Load(),
		DatabaseBackoff:         time.Duration(h.metrics.backoffNanos.Load()),
	}
}

type subscribeRequest struct {
	after    uuid.UUID
	response chan subscribeResponse
}

type subscribeResponse struct {
	subscription *Subscription
	err          error
}

type subscriber struct {
	id       uint64
	sequence uint64
	events   chan Event
	done     chan error
	once     sync.Once
}

func (s *subscriber) finish(err error) {
	s.once.Do(func() {
		if err != nil {
			s.done <- err
		}
		close(s.done)
		close(s.events)
	})
}

type Subscription struct {
	Events <-chan Event
	Done   <-chan error
	once   sync.Once
	close  func()
}

func (s *Subscription) Close() {
	if s == nil || s.close == nil {
		return
	}
	s.once.Do(s.close)
}

type watcher struct {
	hub         *Hub
	scope       string
	ctx         context.Context
	cancel      context.CancelFunc
	subscribe   chan subscribeRequest
	unsubscribe chan uint64
	done        chan struct{}
	subscribers map[uint64]*subscriber
	nextID      uint64
	cursor      uuid.UUID
	// The watcher fetch cursor and each subscriber's delivered sequence are
	// independent so reconnect replay cannot be skipped by shared fan-out.
	cursorSequence  uint64
	hasCursor       bool
	cursorSequences map[uuid.UUID]uint64
	cursorOrder     []uuid.UUID
	backoff         time.Duration
	backoffUntil    time.Time
	unhealthy       bool
}

func (w *watcher) run() {
	defer func() {
		if w.unhealthy {
			if w.hub.metrics.unhealthy.Add(-1) == 0 {
				w.hub.metrics.backoffNanos.Store(0)
			}
		}
		w.cancel()
		for _, subscriber := range w.subscribers {
			subscriber.finish(ErrClosed)
		}
		w.hub.remove(w.scope, w)
		close(w.done)
	}()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	var poll <-chan time.Time
	for {
		select {
		case <-w.ctx.Done():
			return
		case request := <-w.subscribe:
			if !w.backoffUntil.IsZero() && time.Now().Before(w.backoffUntil) {
				request.response <- subscribeResponse{err: ErrDatabaseBackoff}
				continue
			}
			subscriber, err := w.add(request)
			if err != nil {
				request.response <- subscribeResponse{err: err}
				if len(w.subscribers) == 0 {
					poll = resetTimer(timer, w.backoff)
				}
				continue
			}
			request.response <- subscribeResponse{subscription: w.subscription(subscriber)}
			poll = resetTimer(timer, w.hub.config.PollInterval)
		case id := <-w.unsubscribe:
			if subscriber := w.subscribers[id]; subscriber != nil {
				delete(w.subscribers, id)
				subscriber.finish(nil)
			}
			if len(w.subscribers) == 0 {
				return
			}
		case <-poll:
			if len(w.subscribers) == 0 {
				return
			}
			terminal, err := w.poll()
			if err != nil {
				if !w.unhealthy {
					w.unhealthy = true
					w.hub.metrics.unhealthy.Add(1)
				}
				delay := w.nextBackoff()
				w.backoffUntil = time.Now().Add(delay)
				poll = resetTimer(timer, delay)
				continue
			}
			w.backoff = w.hub.config.PollInterval
			w.backoffUntil = time.Time{}
			if w.unhealthy {
				w.unhealthy = false
				if w.hub.metrics.unhealthy.Add(-1) == 0 {
					w.hub.metrics.backoffNanos.Store(0)
				}
			} else if w.hub.metrics.unhealthy.Load() == 0 {
				w.hub.metrics.backoffNanos.Store(0)
			}
			if terminal || len(w.subscribers) == 0 {
				return
			}
			poll = resetTimer(timer, w.hub.config.PollInterval)
		}
	}
}

func (w *watcher) add(request subscribeRequest) (*subscriber, error) {
	sequence, err := w.resolve(request.after)
	if err != nil {
		return nil, err
	}
	w.nextID++
	subscriber := &subscriber{
		id: w.nextID, sequence: sequence,
		events: make(chan Event, w.hub.config.SubscriberQueue), done: make(chan error, 1),
	}
	if !w.hasCursor || sequence < w.cursorSequence {
		w.cursor = request.after
		w.cursorSequence = sequence
		w.hasCursor = true
	}
	w.subscribers[subscriber.id] = subscriber
	return subscriber, nil
}

func (w *watcher) resolve(cursor uuid.UUID) (uint64, error) {
	if cursor == uuid.Nil {
		return 0, nil
	}
	if sequence, ok := w.cursorSequences[cursor]; ok {
		return sequence, nil
	}
	w.hub.metrics.queries.Add(1)
	sequence, err := w.hub.resolve(w.ctx, w.scope, cursor)
	if err != nil {
		return 0, err
	}
	if sequence == 0 {
		return 0, ErrInvalidCursor
	}
	w.remember(cursor, sequence)
	return sequence, nil
}

func (w *watcher) remember(cursor uuid.UUID, sequence uint64) {
	if cursor == uuid.Nil || sequence == 0 {
		return
	}
	if _, exists := w.cursorSequences[cursor]; exists {
		return
	}
	w.cursorSequences[cursor] = sequence
	w.cursorOrder = append(w.cursorOrder, cursor)
	if len(w.cursorOrder) > w.hub.config.SubscriberQueue {
		oldest := w.cursorOrder[0]
		w.cursorOrder = w.cursorOrder[1:]
		delete(w.cursorSequences, oldest)
	}
}

func (w *watcher) subscription(subscriber *subscriber) *Subscription {
	return &Subscription{
		Events: subscriber.events, Done: subscriber.done,
		close: func() {
			select {
			case w.unsubscribe <- subscriber.id:
			case <-w.done:
			case <-w.ctx.Done():
			}
		},
	}
}

func (w *watcher) poll() (bool, error) {
	if !w.hasCursor {
		return false, nil
	}
	w.hub.metrics.queries.Add(1)
	events, err := w.hub.fetch(w.ctx, w.scope, w.cursor, watcherPageSize)
	if err != nil {
		w.hub.metrics.databaseErrors.Add(1)
		return false, err
	}
	terminal := false
	for _, event := range events {
		if event.Name != "" && (event.ID == uuid.Nil || event.Sequence == 0 || event.Sequence <= w.cursorSequence) {
			return false, errors.New("event source returned a non-advancing durable cursor")
		}
		if event.ID != uuid.Nil {
			w.cursor = event.ID
			w.cursorSequence = event.Sequence
			w.remember(event.ID, event.Sequence)
		}
		if event.Terminal {
			terminal = true
		}
		if event.Name == "" {
			continue
		}
		for id, subscriber := range w.subscribers {
			if event.Sequence <= subscriber.sequence {
				continue
			}
			select {
			case subscriber.events <- cloneEvent(event):
				subscriber.sequence = event.Sequence
			default:
				delete(w.subscribers, id)
				w.hub.metrics.slowConsumers.Add(1)
				subscriber.finish(ErrSlowConsumer)
			}
		}
	}
	if terminal {
		for id, subscriber := range w.subscribers {
			delete(w.subscribers, id)
			subscriber.finish(nil)
		}
	}
	return terminal, nil
}

func (w *watcher) nextBackoff() time.Duration {
	next := w.backoff * 2
	if next < w.hub.config.PollInterval {
		next = w.hub.config.PollInterval
	}
	if next > w.hub.config.DatabaseMaxBackoff {
		next = w.hub.config.DatabaseMaxBackoff
	}
	// A fixed 20 percent window prevents synchronized replicas without making
	// test timing depend on an unbounded random source.
	window := next / 5
	if window > 0 {
		next = next - window + time.Duration(mathrand.Uint64N(uint64(2*window)+1))
	}
	if next > w.hub.config.DatabaseMaxBackoff {
		next = w.hub.config.DatabaseMaxBackoff
	}
	w.backoff = next
	w.hub.metrics.backoffNanos.Store(int64(next))
	return next
}

func resetTimer(timer *time.Timer, duration time.Duration) <-chan time.Time {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
	return timer.C
}

func cloneEvent(event Event) Event {
	cloned := event
	cloned.Data = append([]byte(nil), event.Data...)
	return cloned
}
