package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/google/uuid"
)

func TestShutdownWaitsForTimedOutHandler(t *testing.T) {
	s := NewBackend("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Millisecond, false, "")
	release, finished := make(chan struct{}), make(chan struct{})
	defer close(release)
	handler := s.timeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		<-r.Context().Done()
		<-release
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatal("request did not time out", response.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown ignored inner database handler: %v", err)
	}
	// Cleanup the deliberately stalled test handler without leaving a waiter.
	t.Cleanup(func() {
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("inner handler did not finish")
		}
	})
}

func TestShutdownDoesNotReopenEventStreams(t *testing.T) {
	s := NewBackend("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	_, admission, hub := s.eventStreamComponents(false)
	if _, err := admission.Acquire(eventstream.AdmissionKey{Identity: "late", Session: "late", Workspace: "late", Resource: "late"}); !errors.Is(err, eventstream.ErrClosed) {
		t.Fatalf("shutdown admitted late request: %v", err)
	}
	if _, err := hub.Subscribe(ctx, "late", uuid.Nil); !errors.Is(err, eventstream.ErrClosed) {
		t.Fatalf("shutdown restarted database watcher: %v", err)
	}
}

func TestShutdownTimeoutClosesConnections(t *testing.T) {
	s := NewBackend("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "")
	started, finished := make(chan struct{}), make(chan struct{})
	s.http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(finished)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.http.Serve(listener) }()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + listener.Addr().String())
		if err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown timeout: %v", err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("timed-out shutdown left an active connection")
	}
	<-requestDone
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}
