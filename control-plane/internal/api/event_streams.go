package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/google/uuid"
)

// ConfigureEventStreams installs validated capacity limits before the HTTP
// server starts. Reconfiguration closes any old watcher set instead of leaving
// orphaned database polling behind.
func (s *Server) ConfigureEventStreams(config eventstream.Config) error {
	return s.configureEventStreams(config)
}

func (s *Server) configureEventStreams(config eventstream.Config) error {
	manager, err := eventstream.NewManager(config)
	if err != nil {
		return err
	}
	platformHub, err := eventstream.NewHub(config, s.fetchPlatformEvents, s.resolvePlatformEventCursor)
	if err != nil {
		return err
	}
	operationHub, err := eventstream.NewHub(config, s.fetchOperationEvents, s.resolveOperationEventCursor)
	if err != nil {
		platformHub.Close()
		return err
	}
	s.eventStreamsMu.Lock()
	oldManager, oldPlatform, oldOperation := s.eventAdmission, s.platformEvents, s.operationEvents
	s.eventConfig, s.eventAdmission, s.platformEvents, s.operationEvents = config, manager, platformHub, operationHub
	s.eventStreamsMu.Unlock()
	if oldManager != nil {
		oldManager.Close()
	}
	if oldPlatform != nil {
		oldPlatform.Close()
	}
	if oldOperation != nil {
		oldOperation.Close()
	}
	return nil
}

func (s *Server) eventStreamComponents(operation bool) (eventstream.Config, *eventstream.Manager, *eventstream.Hub) {
	s.eventStreamsMu.Lock()
	defer s.eventStreamsMu.Unlock()
	if s.eventAdmission == nil {
		config := eventstream.DefaultConfig()
		manager, err := eventstream.NewManager(config)
		if err != nil {
			panic(err)
		}
		platform, err := eventstream.NewHub(config, s.fetchPlatformEvents, s.resolvePlatformEventCursor)
		if err != nil {
			panic(err)
		}
		operations, err := eventstream.NewHub(config, s.fetchOperationEvents, s.resolveOperationEventCursor)
		if err != nil {
			platform.Close()
			panic(err)
		}
		s.eventConfig, s.eventAdmission, s.platformEvents, s.operationEvents = config, manager, platform, operations
	}
	if operation {
		return s.eventConfig, s.eventAdmission, s.operationEvents
	}
	return s.eventConfig, s.eventAdmission, s.platformEvents
}

func (s *Server) closeEventStreams() {
	s.eventStreamsMu.Lock()
	manager, platform, operations := s.eventAdmission, s.platformEvents, s.operationEvents
	s.eventAdmission, s.platformEvents, s.operationEvents = nil, nil, nil
	s.eventStreamsMu.Unlock()
	if manager != nil {
		manager.Close()
	}
	if platform != nil {
		platform.Close()
	}
	if operations != nil {
		operations.Close()
	}
}

func (s *Server) eventStreamSnapshots() (eventstream.AdmissionSnapshot, eventstream.HubSnapshot, eventstream.HubSnapshot) {
	s.eventStreamsMu.Lock()
	defer s.eventStreamsMu.Unlock()
	var admission eventstream.AdmissionSnapshot
	var platform, operations eventstream.HubSnapshot
	if s.eventAdmission != nil {
		admission = s.eventAdmission.Snapshot()
	}
	if s.platformEvents != nil {
		platform = s.platformEvents.Snapshot()
	}
	if s.operationEvents != nil {
		operations = s.operationEvents.Snapshot()
	}
	return admission, platform, operations
}

func (s *Server) fetchPlatformEvents(ctx context.Context, scope string, after uuid.UUID, limit int) ([]eventstream.Event, error) {
	workspaceID, err := uuid.Parse(scope)
	if err != nil || workspaceID.Version() != 7 {
		return nil, errors.New("event watcher workspace is invalid")
	}
	service := s.localSliceService()
	if service == nil {
		return nil, errors.New("platform event source is unavailable")
	}
	events, _, err := service.ListEventsInWorkspace(ctx, workspaceID, after, limit)
	if err != nil {
		return nil, err
	}
	result := make([]eventstream.Event, 0, len(events))
	for _, event := range events {
		id, err := uuid.Parse(event.ID)
		if err != nil || id.Version() != 7 {
			return nil, errors.New("stored platform event identifier is invalid")
		}
		data, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		if event.Sequence <= 0 {
			return nil, errors.New("stored platform event sequence is invalid")
		}
		result = append(result, eventstream.Event{ID: id, Sequence: uint64(event.Sequence), Name: "platform", Data: data})
	}
	return result, nil
}

func (s *Server) resolvePlatformEventCursor(ctx context.Context, scope string, cursor uuid.UUID) (uint64, error) {
	workspaceID, err := uuid.Parse(scope)
	if err != nil || workspaceID.Version() != 7 {
		return 0, eventstream.ErrInvalidCursor
	}
	service := s.localSliceService()
	if service == nil {
		return 0, errors.New("platform event source is unavailable")
	}
	sequence, visible, err := service.EventSequenceInWorkspace(ctx, workspaceID, cursor)
	if err != nil {
		return 0, err
	}
	if !visible || sequence <= 0 {
		return 0, eventstream.ErrInvalidCursor
	}
	return uint64(sequence), nil
}

func (s *Server) fetchOperationEvents(ctx context.Context, scope string, after uuid.UUID, limit int) ([]eventstream.Event, error) {
	operationID, err := uuid.Parse(scope)
	if err != nil || operationID.Version() != 7 || s.operations == nil {
		return nil, errors.New("operation event source is unavailable")
	}
	events, err := s.operations.ListEvents(ctx, operationID, after, limit)
	if err != nil {
		return nil, err
	}
	result := make([]eventstream.Event, 0, len(events)+1)
	terminal := false
	for _, event := range events {
		id, err := uuid.Parse(event.ID)
		if err != nil || id.Version() != 7 {
			return nil, errors.New("stored operation event identifier is invalid")
		}
		data, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		terminal = operationStateTerminal(event.State)
		if event.Sequence <= 0 {
			return nil, errors.New("stored operation event sequence is invalid")
		}
		result = append(result, eventstream.Event{ID: id, Sequence: uint64(event.Sequence), Name: "operation", Data: data, Terminal: terminal})
	}
	if len(result) == 0 {
		operation, err := s.operations.Get(ctx, operationID)
		if err != nil {
			return nil, err
		}
		if operationStateTerminal(operation.State) {
			result = append(result, eventstream.Event{Terminal: true})
		}
	}
	return result, nil
}

func (s *Server) resolveOperationEventCursor(ctx context.Context, scope string, cursor uuid.UUID) (uint64, error) {
	operationID, err := uuid.Parse(scope)
	if err != nil || operationID.Version() != 7 || s.operations == nil {
		return 0, eventstream.ErrInvalidCursor
	}
	sequence, visible, err := s.operations.EventSequence(ctx, operationID, cursor)
	if err != nil {
		return 0, err
	}
	if !visible || sequence <= 0 {
		return 0, eventstream.ErrInvalidCursor
	}
	return uint64(sequence), nil
}

func operationStateTerminal(state string) bool {
	switch state {
	case "draft", "queued", "dispatched", "accepted", "running", "unknown", "offline_pending":
		return false
	default:
		return state != ""
	}
}

func (s *Server) serveEventStream(w http.ResponseWriter, r *http.Request, flusher http.Flusher, operation bool, scope, resource string, after uuid.UUID) {
	config, admission, hub := s.eventStreamComponents(operation)
	actor := principal(r)
	key := eventAdmissionKey(actor, workspace(r), resource)
	lease, err := admission.Acquire(key)
	if err != nil {
		s.writeStreamAdmissionError(w, r, config, err)
		return
	}
	defer lease.Release()

	lifetime, err := eventStreamLifetime(config, actor.ExpiresAt, time.Now())
	if err != nil {
		s.writeStreamAdmissionError(w, r, config, err)
		return
	}
	subscription, err := hub.Subscribe(r.Context(), scope, after)
	if err != nil {
		s.writeStreamAdmissionError(w, r, config, err)
		return
	}
	defer subscription.Close()

	ctx, cancel := context.WithTimeout(r.Context(), lifetime)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()
	controller := http.NewResponseController(w)
	keepalive := time.NewTicker(10 * time.Second)
	revalidate := time.NewTicker(config.RevalidateInterval)
	defer keepalive.Stop()
	defer revalidate.Stop()
	streamDone := subscription.Done
	for {
		select {
		case <-ctx.Done():
			return
		case streamErr, ok := <-streamDone:
			if ok && streamErr != nil {
				return
			}
			// A graceful terminal watcher closes Done only after queueing the
			// durable tail. Disable this arm and drain Events before returning.
			streamDone = nil
		case event, ok := <-subscription.Events:
			if !ok {
				return
			}
			_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", event.ID, event.Name, event.Data); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-revalidate.C:
			if !s.revalidateEventStream(ctx, r, actor) {
				s.logger.WarnContext(ctx, "event stream authorization revoked", "request_id", requestID(r))
				return
			}
		}
	}
}

func eventStreamLifetime(config eventstream.Config, sessionExpiresAt, now time.Time) (time.Duration, error) {
	lifetime := config.MaxLifetime
	if sessionExpiresAt.IsZero() {
		return lifetime, nil
	}
	remaining := sessionExpiresAt.Sub(now)
	if remaining <= 0 {
		return 0, auth.ErrUnauthenticated
	}
	if remaining < lifetime {
		lifetime = remaining
	}
	return lifetime, nil
}

func eventStreamCursor(r *http.Request) (uuid.UUID, bool) {
	cursor := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if cursor == "" {
		cursor = strings.TrimSpace(r.URL.Query().Get("after"))
	}
	return parseEventID(cursor)
}

func eventAdmissionKey(principal auth.Principal, workspaceID uuid.UUID, resource string) eventstream.AdmissionKey {
	identity, session := principal.IdentityID.String(), principal.SessionID.String()
	if principal.IdentityID == uuid.Nil {
		identity = "development"
	}
	if principal.SessionID == uuid.Nil {
		session = "development"
	}
	return eventstream.AdmissionKey{Identity: identity, Session: session, Workspace: workspaceID.String(), Resource: resource}
}

func (s *Server) revalidateEventStream(ctx context.Context, request *http.Request, expected auth.Principal) bool {
	copy := request.Clone(ctx)
	current, err := s.authenticate(copy)
	if err != nil || current.IdentityID != expected.IdentityID || current.SessionID != expected.SessionID {
		return false
	}
	_, err = s.authorizeRoute(copy, current)
	return err == nil
}

func (s *Server) writeStreamAdmissionError(w http.ResponseWriter, r *http.Request, config eventstream.Config, err error) {
	w.Header().Set("Retry-After", strconv.Itoa(int((config.RetryAfter+time.Second-1)/time.Second)))
	if errors.Is(err, eventstream.ErrInvalidCursor) {
		w.Header().Del("Retry-After")
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-cursor", "Cursor is invalid", "Last-Event-ID is not visible in the authorized event scope")
		return
	}
	if errors.Is(err, eventstream.ErrGlobalLimit) || errors.Is(err, eventstream.ErrWatcherLimit) || errors.Is(err, eventstream.ErrDatabaseBackoff) || errors.Is(err, eventstream.ErrClosed) {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/event-stream-overloaded", "Event stream unavailable", "event stream capacity is temporarily unavailable")
		return
	}
	if errors.Is(err, auth.ErrUnauthenticated) {
		writeProblem(w, r, http.StatusUnauthorized, "https://ocservia.dev/problems/unauthenticated", "Authentication required", "the authenticated session has expired")
		return
	}
	writeProblem(w, r, http.StatusTooManyRequests, "https://ocservia.dev/problems/event-stream-limit", "Event stream limit reached", "the principal or resource event stream limit was reached")
}
