package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/trace"
)

func (s *Server) createSimulation(w http.ResponseWriter, r *http.Request) {
	service := s.localSliceService()
	if service == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	var scenario localslice.Scenario
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&scenario); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the simulation request is invalid")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the simulation request must contain one JSON object")
		return
	}
	requestID, _ := r.Context().Value(requestIDKey{}).(string)
	operation, err := service.Create(r.Context(), scenario, requestID, requestTraceparent(r))
	if err != nil {
		s.logger.ErrorContext(r.Context(), "create local simulation", "error", err)
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-simulation", "Simulation is invalid", "the simulation could not be accepted")
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	service := s.localSliceService()
	if service == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	id, err := uuid.Parse(r.PathValue("operation_id"))
	if err != nil || id.Version() != 7 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "operation_id must be a UUIDv7")
		return
	}
	operation, err := service.GetOperation(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested operation does not exist")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "get operation", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "operation state is temporarily unavailable")
		return
	}
	writeJSON(w, http.StatusOK, operation)
}

func (s *Server) listOperations(w http.ResponseWriter, r *http.Request) {
	service := s.localSliceService()
	if service == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	operations, err := service.ListOperations(r.Context(), 200)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "list operations", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "operations are temporarily unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": operations, "page": map[string]bool{"has_more": false}})
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	service := s.localSliceService()
	if service == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	after, ok := parseEventID(r.URL.Query().Get("after"))
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-cursor", "Cursor is invalid", "after must be a UUIDv7 event ID")
		return
	}
	limit := 100
	if value := r.URL.Query().Get("page_size"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-page-size", "Page size is invalid", "page_size must be an integer")
			return
		}
		limit = parsed
	}
	events, err := service.ListEvents(r.Context(), after, limit)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "list events", "error", err)
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-event-query", "Event query is invalid", "events could not be listed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": events, "page": map[string]bool{"has_more": false}})
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	service := s.localSliceService()
	if service == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, r, http.StatusInternalServerError, "https://ocservia.dev/problems/stream-unavailable", "Stream is unavailable", "streaming is not supported")
		return
	}
	after, valid := parseEventID(strings.TrimSpace(r.Header.Get("Last-Event-ID")))
	if !valid {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-cursor", "Cursor is invalid", "Last-Event-ID must be a UUIDv7")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()
	ticker := time.NewTicker(250 * time.Millisecond)
	keepalive := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	defer keepalive.Stop()
	controller := http.NewResponseController(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			events, err := service.ListEvents(r.Context(), after, 100)
			if err != nil {
				s.logger.WarnContext(r.Context(), "poll SSE events", "error", err)
				continue
			}
			for _, event := range events {
				data, err := json.Marshal(event)
				if err != nil {
					return
				}
				_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if _, err := fmt.Fprintf(w, "id: %s\nevent: platform\ndata: %s\n\n", event.ID, data); err != nil {
					return
				}
				flusher.Flush()
				after, _ = uuid.Parse(event.ID)
			}
		}
	}
}

func requestTraceparent(r *http.Request) string {
	span := trace.SpanContextFromContext(r.Context())
	if span.IsValid() {
		flags := "00"
		if span.IsSampled() {
			flags = "01"
		}
		return fmt.Sprintf("00-%s-%s-%s", span.TraceID(), span.SpanID(), flags)
	}
	traceID := correlationHex(32)
	spanID := correlationHex(16)
	return "00-" + traceID + "-" + spanID + "-01"
}

func correlationHex(length int) string {
	id, err := uuid.NewV7()
	if err != nil {
		return strings.Repeat("1", length)
	}
	return strings.ReplaceAll(id.String(), "-", "")[:length]
}

func parseEventID(value string) (uuid.UUID, bool) {
	if value == "" {
		return uuid.Nil, true
	}
	id, err := uuid.Parse(value)
	return id, err == nil && id.Version() == 7
}
