package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
)

type baselineBlockedDatabase struct {
	database.Backend
	started, release, finished chan struct{}
}

func (b *baselineBlockedDatabase) Ping(ctx context.Context) error {
	close(b.started)
	<-ctx.Done()
	<-b.release
	close(b.finished)
	return ctx.Err()
}
func (*baselineBlockedDatabase) PoolStats() database.PoolStats { return database.PoolStats{} }
func (*baselineBlockedDatabase) ControllerSchema(context.Context, int64) (int64, error) {
	return 36, nil
}

func TestHTTPTimeoutAndSSEBaseline(t *testing.T) {
	b := &baselineBlockedDatabase{started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	s := NewBackend("127.0.0.1:0", b, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, 20*time.Millisecond, true, "", 36)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(b.release) }) }
	t.Cleanup(func() { release(); _ = s.Shutdown(context.Background()) })
	s.EnableOperations(&operations.Service{})
	first, second := uuid.MustParse(baselineID), uuid.MustParse("019fc0a4-6d92-765c-a8a1-4af556614cc4")
	emit := make(chan struct{})
	config := eventstream.DefaultConfig()
	config.PollInterval = 100 * time.Millisecond
	hub, err := eventstream.NewHub(config, func(ctx context.Context, scope string, after uuid.UUID, limit int) ([]eventstream.Event, error) {
		select {
		case <-emit:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if after == second {
			return nil, nil
		}
		return []eventstream.Event{{ID: second, Sequence: 2, Name: "operation", Data: []byte(`{"state":"running"}`)}}, nil
	}, func(ctx context.Context, scope string, id uuid.UUID) (uint64, error) {
		if id == first {
			return 1, nil
		}
		if id == second {
			return 2, nil
		}
		return 0, eventstream.ErrInvalidCursor
	})
	if err != nil {
		t.Fatal(err)
	}
	s.operationEvents.Close()
	s.operationEvents = hub
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
	for _, want := range []string{"id: " + second.String() + "\n", "event: operation\n", "data: {\"state\":\"running\"}\n", "\n"} {
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
