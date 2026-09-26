package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

type baselineBlockedDatabase struct {
	database.Backend
	started, release, finished chan struct{}
	events                     *baselineEventStore
}

func (b *baselineBlockedDatabase) Begin(context.Context, database.Isolation) (database.Tx, error) {
	return baselineEventTx{events: b.events}, nil
}

type baselineEventTx struct {
	database.Tx
	events *baselineEventStore
}

func (tx baselineEventTx) Commit(context.Context) error         { return nil }
func (tx baselineEventTx) Rollback(context.Context) error       { return nil }
func (tx baselineEventTx) OperationStore() operationstore.Store { return tx.events }

type baselineEventStore struct {
	operationstore.Store
	emit          chan struct{}
	first, second uuid.UUID
}

func (s *baselineEventStore) Events(ctx context.Context, operationID, after uuid.UUID, _ int) ([]operationstore.Event, error) {
	select {
	case <-s.emit:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if after == s.second {
		return nil, nil
	}
	return []operationstore.Event{{ID: s.second.String(), OperationID: operationID.String(), Sequence: 2, State: "running"}}, nil
}

func (s *baselineEventStore) EventSequence(_ context.Context, _, id uuid.UUID) (int64, error) {
	if id == s.first {
		return 1, nil
	}
	if id == s.second {
		return 2, nil
	}
	return 0, database.ErrNotFound
}

func (s *baselineEventStore) Get(context.Context, uuid.UUID) (operationstore.Operation, error) {
	return operationstore.Operation{State: "running"}, nil
}

func (b *baselineBlockedDatabase) CheckReadiness(ctx context.Context) error {
	close(b.started)
	<-ctx.Done()
	<-b.release
	close(b.finished)
	return ctx.Err()
}
func (*baselineBlockedDatabase) PoolStats() database.PoolStats { return database.PoolStats{} }

func TestHTTPTimeoutAndSSEBaseline(t *testing.T) {
	b := &baselineBlockedDatabase{started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	config := testHTTPConfig(true)
	config.BodyLimit, config.RequestTimeout = 1024, 20*time.Millisecond
	config.EventStreams.PollInterval = 100 * time.Millisecond
	s := newTestServer(t, config, b, Modules{}, Authorization{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(b.release) }) }
	t.Cleanup(func() { release(); _ = s.Shutdown(context.Background()) })
	s.EnableOperations(operations.NewBackend(b, 50, nil))
	first, second := uuid.MustParse(baselineID), uuid.MustParse("019fc0a4-6d92-765c-a8a1-4af556614cc4")
	emit := make(chan struct{})
	b.events = &baselineEventStore{emit: emit, first: first, second: second}
	hub := s.operationEvents
	server := httptest.NewServer(s.http.Handler)
	t.Cleanup(server.Close)
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	path := "/api/v1/operations/" + baselineID + "/events"
	r, _ := http.NewRequest("GET", server.URL+path+"?after=invalid", nil)
	r.Header.Set("Last-Event-ID", first.String())
	r.Header.Set("X-Request-ID", "stream-baseline")
	stream, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != 200 || stream.Header.Get("Content-Type") != "text/event-stream" || stream.Header.Get("Cache-Control") != "no-cache" || stream.Header.Get("X-Accel-Buffering") != "no" || stream.Header.Get("X-Request-ID") != "stream-baseline" {
		t.Fatalf("stream headers: %d %v", stream.StatusCode, stream.Header)
	}
	// Headers have flushed before any event. A normal request now times out;
	// only afterwards may the watcher emit, proving SSE outlives that deadline.
	response, err := client.Get(server.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 503 || response.Header.Get("Content-Type") != "application/problem+json" || string(body) != `{"type":"https://ocservia.dev/problems/timeout","title":"Request timed out","status":503}` {
		t.Fatalf("timeout: %d %v %q %v", response.StatusCode, response.Header, body, err)
	}
	close(emit)
	reader := bufio.NewReader(stream.Body)
	eventJSON, err := json.Marshal(operationstore.Event{ID: second.String(), OperationID: baselineID, State: "running"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id: " + second.String() + "\n", "event: operation\n", "data: " + string(eventJSON) + "\n", "\n"} {
		line, err := reader.ReadString('\n')
		if err != nil || line != want {
			t.Fatalf("SSE frame: %q want %q: %v", line, want, err)
		}
	}
	stream.Body.Close()
	// Resume through the query cursor, then close the hub via real Shutdown.
	resumed, err := client.Get(server.URL + path + "?after=" + second.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Body.Close()
	if resumed.StatusCode != 200 {
		t.Fatalf("resume: %d", resumed.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown forgot timed-out inner work: %v", err)
	}
	tail, err := io.ReadAll(resumed.Body)
	if err != nil || len(tail) != 0 {
		t.Fatalf("resume replayed an event or failed to close: %q %v", tail, err)
	}
	release()
	select {
	case <-b.finished:
	case <-time.After(time.Second):
		t.Fatal("database handler did not return")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	late := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(late, baselineRequest("GET", path, nil))
	if late.Code != 503 || late.Body.Len() != 0 {
		t.Fatalf("late stream: %d %s", late.Code, late.Body)
	}
	if _, err := hub.Subscribe(context.Background(), baselineID, first); !errors.Is(err, eventstream.ErrClosed) {
		t.Fatalf("watcher reopened: %v", err)
	}
}

func TestHTTPShutdownAdmissionBaseline(t *testing.T) {
	s := baselineServer(t, false)
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 32)
	for i := 0; i < cap(results); i++ {
		go func() {
			<-start
			w := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, baselineRequest("GET", "/livez", nil))
			results <- w
		}()
	}
	done := make(chan error, 1)
	go func() { <-start; done <- s.Shutdown(context.Background()) }()
	close(start)
	for i := 0; i < cap(results); i++ {
		select {
		case w := <-results:
			if w.Code == 200 {
				if w.Body.String() != "{\"status\":\"ok\"}\n" {
					t.Fatal(w.Body)
				}
			} else if w.Code != 503 || w.Body.Len() != 0 {
				t.Fatalf("admission: %d %s", w.Code, w.Body)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("request did not drain")
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish")
	}
	w := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(w, baselineRequest("GET", "/livez", nil))
	if w.Code != 503 || w.Body.Len() != 0 {
		t.Fatalf("post-shutdown admission: %d %s", w.Code, w.Body)
	}
}
