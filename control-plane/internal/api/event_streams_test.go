package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/google/uuid"
)

func TestStreamAdmissionWrites429BeforeSSEHeaders(t *testing.T) {
	server, config, principal, workspaceID := streamAdmissionFixture(t)
	key := eventAdmissionKey(principal, workspaceID, "held")
	lease, err := server.eventAdmission.Acquire(key)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	request := authorizedStreamRequest(principal, workspaceID)
	response := httptest.NewRecorder()
	server.serveEventStream(response, request, response, false, workspaceID.String(), "candidate", uuid.Nil)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("status=%d retry-after=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body)
	}
	if response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
	}
	if snapshot := server.platformEvents.Snapshot(); snapshot.Watchers != 0 || snapshot.Queries != 0 {
		t.Fatalf("rejected stream started database work: %+v", snapshot)
	}
	if config.IdentityStreams != 1 {
		t.Fatal("fixture did not exercise the identity limit")
	}
}

func TestStreamGlobalOverloadWrites503BeforeSSEHeaders(t *testing.T) {
	server, _, principal, workspaceID := streamAdmissionFixture(t)
	server.eventStreamsMu.Lock()
	config := server.eventConfig
	server.eventStreamsMu.Unlock()
	config.GlobalStreams = 1
	config.IdentityStreams = 1
	config.SessionStreams = 1
	config.WorkspaceStreams = 1
	config.ResourceStreams = 1
	config.Watchers = 1
	if err := server.ConfigureEventStreams(config); err != nil {
		t.Fatal(err)
	}
	lease, err := server.eventAdmission.Acquire(eventAdmissionKey(principal, workspaceID, "held"))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	request := authorizedStreamRequest(principal, workspaceID)
	response := httptest.NewRecorder()
	server.serveEventStream(response, request, response, false, workspaceID.String(), "candidate", uuid.Nil)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("status=%d retry-after=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body)
	}
	if snapshot := server.platformEvents.Snapshot(); snapshot.Watchers != 0 || snapshot.Queries != 0 {
		t.Fatalf("globally rejected stream started database work: %+v", snapshot)
	}
}

func TestInvisibleLastEventIDWrites400BeforeSSEHeaders(t *testing.T) {
	server, _, principal, workspaceID := streamAdmissionFixture(t)
	request := authorizedStreamRequest(principal, workspaceID)
	response := httptest.NewRecorder()
	server.serveEventStream(response, request, response, true, uuid.Must(uuid.NewV7()).String(), "operation", uuid.Must(uuid.NewV7()))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
	}
	if response.Header().Get("Retry-After") != "" {
		t.Fatalf("invalid cursor retry-after = %q", response.Header().Get("Retry-After"))
	}
	if snapshot := server.operationEvents.Snapshot(); snapshot.Queries != 1 {
		t.Fatalf("invalid cursor database work was not limited to one visibility lookup: %+v", snapshot)
	}
}

func TestEventStreamLifetimeCannotOutliveSession(t *testing.T) {
	config := eventstream.DefaultConfig()
	now := time.Unix(1_700_000_000, 0)
	remaining := 2 * time.Minute
	lifetime, err := eventStreamLifetime(config, now.Add(remaining), now)
	if err != nil || lifetime != remaining {
		t.Fatalf("session-bounded lifetime = %v, %v", lifetime, err)
	}
	lifetime, err = eventStreamLifetime(config, now.Add(2*config.MaxLifetime), now)
	if err != nil || lifetime != config.MaxLifetime {
		t.Fatalf("configured lifetime = %v, %v", lifetime, err)
	}
	if _, err := eventStreamLifetime(config, now, now); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("expired session error = %v", err)
	}
}

func TestEventStreamCursorSupportsAfterAndLastEventID(t *testing.T) {
	after := uuid.Must(uuid.NewV7())
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream?after="+after.String(), nil)
	parsed, ok := eventStreamCursor(request)
	if !ok || parsed != after {
		t.Fatalf("after cursor = %s, %v", parsed, ok)
	}
	last := uuid.Must(uuid.NewV7())
	request.Header.Set("Last-Event-ID", last.String())
	parsed, ok = eventStreamCursor(request)
	if !ok || parsed != last {
		t.Fatalf("Last-Event-ID cursor = %s, %v", parsed, ok)
	}
	request.Header.Set("Last-Event-ID", "not-a-uuid")
	if _, ok := eventStreamCursor(request); ok {
		t.Fatal("invalid Last-Event-ID fell back to query cursor")
	}
}

func streamAdmissionFixture(t *testing.T) (*Server, eventstream.Config, auth.Principal, uuid.UUID) {
	t.Helper()
	server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	config := eventstream.DefaultConfig()
	config.GlobalStreams = 2
	config.IdentityStreams = 1
	config.SessionStreams = 1
	config.WorkspaceStreams = 2
	config.ResourceStreams = 2
	config.Watchers = 2
	if err := server.ConfigureEventStreams(config); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.closeEventStreams)
	principal := auth.Principal{IdentityID: uuid.Must(uuid.NewV7()), SessionID: uuid.Must(uuid.NewV7()), ExpiresAt: time.Now().Add(time.Hour)}
	return server, config, principal, uuid.Must(uuid.NewV7())
}

func authorizedStreamRequest(principal auth.Principal, workspaceID uuid.UUID) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil)
	ctx := context.WithValue(request.Context(), principalKey{}, principal)
	ctx = context.WithValue(ctx, workspaceKey{}, workspaceID)
	return request.WithContext(ctx)
}
