package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	if s.telemetry == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/telemetry-unavailable", "Telemetry unavailable", "the telemetry read model is unavailable")
		return
	}
	after, ok := parseEventID(r.URL.Query().Get("cursor"))
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-cursor", "Invalid cursor", "cursor must be a UUIDv7 node ID")
		return
	}
	limit, ok := pageSize(r, 50)
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-page-size", "Invalid page size", "page_size must be between 1 and 200")
		return
	}
	nodes, hasMore, err := s.telemetry.ListNodes(r.Context(), after, limit)
	if err != nil {
		s.logger.Error("list nodes failed", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Nodes unavailable", "node state could not be read")
		return
	}
	page := map[string]any{"has_more": hasMore}
	if hasMore && len(nodes) > 0 {
		page["next_cursor"] = nodes[len(nodes)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nodes, "page": page})
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodePathID(w, r)
	if !ok {
		return
	}
	node, err := s.telemetry.GetNode(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Node not found", "the requested node does not exist")
		return
	}
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Node unavailable", "node state could not be read")
		return
	}
	writeJSON(w, http.StatusOK, node)
}

func (s *Server) listNodeSessions(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodePathID(w, r)
	if !ok {
		return
	}
	cursor := r.URL.Query().Get("cursor")
	limit, valid := pageSize(r, 50)
	if !valid || len(cursor) > 256 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-pagination", "Invalid pagination", "cursor and page_size are outside permitted bounds")
		return
	}
	items, hasMore, err := s.telemetry.ListSessions(r.Context(), id, cursor, limit)
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Sessions unavailable", "session state could not be read")
		return
	}
	page := map[string]any{"has_more": hasMore}
	if hasMore && len(items) > 0 {
		page["next_cursor"] = items[len(items)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "page": page})
}

func (s *Server) listNodeTelemetry(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodePathID(w, r)
	if !ok {
		return
	}
	metric := r.URL.Query().Get("metric")
	resolution := r.URL.Query().Get("resolution")
	if resolution == "" {
		resolution = "5m"
	}
	since := time.Time{}
	if value := r.URL.Query().Get("since"); value != "" {
		var err error
		since, err = time.Parse(time.RFC3339, value)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-query", "Invalid query", "since must be an RFC 3339 timestamp")
			return
		}
	}
	items, err := s.telemetry.History(r.Context(), id, metric, resolution, since)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-query", "Invalid query", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "resolution": resolution})
}

func (s *Server) nodePathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	if s.telemetry == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/telemetry-unavailable", "Telemetry unavailable", "the telemetry read model is unavailable")
		return uuid.Nil, false
	}
	id, err := uuid.Parse(r.PathValue("node_id"))
	if err != nil || id.Version() != 7 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-node-id", "Invalid node ID", "node_id must be a UUIDv7")
		return uuid.Nil, false
	}
	return id, true
}
