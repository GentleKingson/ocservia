package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
)

type blockedRequest struct {
	started  chan context.Context
	canceled chan error
	release  chan struct{}
}

func (b *blockedRequest) wait(ctx context.Context) error {
	b.started <- ctx
	<-ctx.Done()
	b.canceled <- ctx.Err()
	<-b.release
	return ctx.Err()
}

func lifetimeJSONRequest(method, path, body string) func() *http.Request {
	return func() *http.Request {
		req := baselineRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "context-test")
		return req
	}
}

// Each subtest verifies cancellation reaches the module while Shutdown still
// waits for the blocked method, even after the HTTP response has returned.
func checkRequestLifetime(t *testing.T, prefix string, request func() *http.Request, modules func(*blockedRequest) Modules, auth Authorization) {
	t.Helper()
	for _, cancelRequest := range []bool{false, true} {
		name := "deadline"
		if cancelRequest {
			name = "cancellation"
		}
		t.Run(prefix+name, func(t *testing.T) {
			timeout := 30 * time.Millisecond
			if cancelRequest {
				timeout = time.Second
			}
			blocked := &blockedRequest{started: make(chan context.Context, 1), canceled: make(chan error, 1), release: make(chan struct{})}
			config := testHTTPConfig(true)
			config.BodyLimit, config.RequestTimeout = 1024, timeout
			s := newTestServer(t, config, nil, modules(blocked), auth)
			release := sync.OnceFunc(func() { close(blocked.release) })
			t.Cleanup(func() { release(); _ = s.Shutdown(context.Background()) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				s.http.Handler.ServeHTTP(w, request().WithContext(ctx))
				done <- w
			}()
			select {
			case got := <-blocked.started:
				if _, ok := got.Deadline(); !ok {
					t.Fatal("HTTP deadline lost")
				}
				if got.Value(requestIDKey{}) != "baseline-request" {
					t.Fatal("request context lost")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("business method did not start")
			}
			wantErr := context.DeadlineExceeded
			if cancelRequest {
				cancel()
				wantErr = context.Canceled
			}
			select {
			case got := <-blocked.canceled:
				if !errors.Is(got, wantErr) {
					t.Fatalf("service context: %v", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("service did not observe cancellation")
			}
			select {
			case w := <-done:
				if w.Code != 503 {
					t.Fatal("timeout status", w.Code)
				}
			case <-time.After(time.Second):
				t.Fatal("outer HTTP handler did not return")
			}
			shutdown, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancelShutdown()
			if err := s.Shutdown(shutdown); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("shutdown returned before the business method was released: %v", err)
			}
			release()
			testutil.NoError(t, s.Shutdown(context.Background()))
		})
	}
}
