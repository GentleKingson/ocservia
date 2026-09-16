package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/api/nodehttp"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetryread"
	"github.com/google/uuid"
)

type observedNodeReader struct {
	nodehttp.Reader
	calls atomic.Int64
}

func (r *observedNodeReader) ListNodesInWorkspace(ctx context.Context, ws, after uuid.UUID, limit int) ([]telemetry.Node, bool, error) {
	r.calls.Add(1)
	return r.Reader.ListNodesInWorkspace(ctx, ws, after, limit)
}
func (r *observedNodeReader) GetNode(ctx context.Context, id uuid.UUID) (telemetry.Node, error) {
	r.calls.Add(1)
	return r.Reader.GetNode(ctx, id)
}
func (r *observedNodeReader) ListSessions(ctx context.Context, id uuid.UUID, cursor string, limit int) ([]telemetryread.Session, bool, error) {
	r.calls.Add(1)
	return r.Reader.ListSessions(ctx, id, cursor, limit)
}
func (r *observedNodeReader) ListIPBans(ctx context.Context, id uuid.UUID, limit int) ([]telemetry.IPBan, error) {
	r.calls.Add(1)
	return r.Reader.ListIPBans(ctx, id, limit)
}
func (r *observedNodeReader) HistoryFrom(ctx context.Context, id uuid.UUID, metric, resolution string, since value.Timestamp) ([]telemetry.HistoryPoint, error) {
	r.calls.Add(1)
	return r.Reader.HistoryFrom(ctx, id, metric, resolution, since)
}

type blockedNodeReader struct {
	nodehttp.Reader
	started  chan context.Context
	canceled chan error
	release  chan struct{}
}

func (r *blockedNodeReader) GetNode(ctx context.Context, _ uuid.UUID) (telemetry.Node, error) {
	r.started <- ctx
	<-ctx.Done()
	r.canceled <- ctx.Err()
	<-r.release
	return telemetry.Node{}, ctx.Err()
}

func TestNodeHTTPContextLifetime(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		name := "deadline"
		if cancelRequest {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			timeout := 30 * time.Millisecond
			if cancelRequest {
				timeout = time.Second
			}
			s := NewBackend("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, timeout, true, "", 36)
			reader := &blockedNodeReader{started: make(chan context.Context, 1), canceled: make(chan error, 1), release: make(chan struct{})}
			var once sync.Once
			release := func() { once.Do(func() { close(reader.release) }) }
			t.Cleanup(func() { release(); _ = s.Shutdown(context.Background()) })
			s.nodeHTTP.SetReader(reader)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				s.http.Handler.ServeHTTP(w, baselineRequest("GET", "/api/v1/nodes/"+baselineID, nil).WithContext(ctx))
				done <- w
			}()
			select {
			case got := <-reader.started:
				if _, ok := got.Deadline(); !ok {
					t.Fatal("HTTP deadline lost")
				}
				if got.Value(requestIDKey{}) != "baseline-request" {
					t.Fatal("request context lost")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("reader did not start")
			}
			wantErr := context.DeadlineExceeded
			if cancelRequest {
				cancel()
				wantErr = context.Canceled
			}
			select {
			case got := <-reader.canceled:
				if !errors.Is(got, wantErr) {
					t.Fatalf("Reader context: %v", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Reader did not observe cancellation")
			}
			select {
			case w := <-done:
				if w.Code != 503 {
					t.Fatalf("timeout status: %d", w.Code)
				}
			case <-time.After(time.Second):
				t.Fatal("outer HTTP handler did not return")
			}
			shutdown, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancelShutdown()
			if err := s.Shutdown(shutdown); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("shutdown forgot node read: %v", err)
			}
			release()
			if err := s.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
