package api

import (
	"net/http"

	"github.com/GentleKingson/ocservia/control-plane/internal/transportevents"
)

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	service := s.transportEvents
	if service == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	after, ok := parseOptionalUUIDv7(r.URL.Query().Get("after"))
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-cursor", "Cursor is invalid", "after must be a UUIDv7 event ID")
		return
	}
	order := r.URL.Query().Get("order")
	if order == "" {
		order = transportevents.ListEventsAscending
	}
	if order != transportevents.ListEventsAscending && order != transportevents.ListEventsDescending {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-order", "Order is invalid", "order must be either asc or desc")
		return
	}
	limit, ok := pageSize(r, 50)
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-page-size", "Page size is invalid", "page_size must be an integer between 1 and 200")
		return
	}
	events, hasMore, err := service.ListEventsInWorkspace(r.Context(), workspace(r), after, limit, order)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "list events", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "events are temporarily unavailable")
		return
	}
	writeCursorPage(w, events, hasMore, func(event transportevents.Event) string { return event.ID })
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	service := s.transportEvents
	if service == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, r, http.StatusInternalServerError, "https://ocservia.dev/problems/stream-unavailable", "Stream is unavailable", "streaming is not supported")
		return
	}
	after, valid := eventStreamCursor(r)
	if !valid {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-cursor", "Cursor is invalid", "Last-Event-ID must be a UUIDv7")
		return
	}
	workspaceID := workspace(r)
	s.serveEventStream(w, r, flusher, false, workspaceID.String(), "workspace-events:"+workspaceID.String(), after)
}
