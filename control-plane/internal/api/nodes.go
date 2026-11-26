package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
	"github.com/google/uuid"
)

type NodeReader interface {
	ListNodesInWorkspace(context.Context, uuid.UUID, uuid.UUID, int) ([]telemetry.Node, bool, error)
	GetNode(context.Context, uuid.UUID) (telemetry.Node, error)
	ListSessions(context.Context, uuid.UUID, string, int) ([]telemetrystore.ReadSession, bool, error)
	ListIPBans(context.Context, uuid.UUID, int) ([]telemetry.IPBan, error)
	History(context.Context, uuid.UUID, string, string, time.Time) ([]telemetry.HistoryPoint, error)
}

var _ NodeReader = (*telemetry.Service)(nil)

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	if s.nodes == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/telemetry-unavailable", "Telemetry unavailable", "the telemetry read model is unavailable")
		return
	}
	after, ok := parseOptionalUUIDv7(r.URL.Query().Get("cursor"))
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-cursor", "Invalid cursor", "cursor must be a UUIDv7 node ID")
		return
	}
	limit, ok := pageSize(r, 50)
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-page-size", "Invalid page size", "page_size must be between 1 and 200")
		return
	}
	nodes, hasMore, err := s.nodes.ListNodesInWorkspace(r.Context(), workspace(r), after, limit)
	if err != nil {
		s.logger.Error("list nodes failed", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Nodes unavailable", "node state could not be read")
		return
	}
	writeCursorPage(w, nodes, hasMore, func(node telemetry.Node) string { return node.ID })
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodePathID(w, r)
	if !ok {
		return
	}
	node, err := s.nodes.GetNode(r.Context(), id)
	if errors.Is(err, database.ErrNotFound) {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Node not found", "the requested node does not exist")
		return
	}
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Node unavailable", "node state could not be read")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
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
	items, hasMore, err := s.nodes.ListSessions(r.Context(), id, cursor, limit)
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Sessions unavailable", "session state could not be read")
		return
	}
	writeCursorPage(w, items, hasMore, func(item telemetrystore.ReadSession) string { return item.ID })
}

func (s *Server) listNodeIPBans(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodePathID(w, r)
	if !ok {
		return
	}
	items, err := s.nodes.ListIPBans(r.Context(), id, 200)
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "IP bans unavailable", "IP ban state could not be read")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
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
	var since time.Time
	if text := r.URL.Query().Get("since"); text != "" {
		var err error
		since, err = time.Parse(time.RFC3339Nano, text)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-query", "Invalid query", "since must be an RFC 3339 timestamp")
			return
		}
	}
	items, err := s.nodes.History(r.Context(), id, metric, resolution, since)
	if err != nil {
		switch {
		case errors.Is(err, telemetry.ErrInvalidMetric):
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-query", "Invalid query", "metric is invalid")
		case errors.Is(err, telemetry.ErrInvalidResolution):
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-query", "Invalid query", "resolution is invalid")
		default:
			s.logger.ErrorContext(r.Context(), "read telemetry history failed", "error", err)
			writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Telemetry unavailable", "telemetry history could not be read")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "resolution": resolution})
}

func (s *Server) nodePathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	if s.nodes == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/telemetry-unavailable", "Telemetry unavailable", "the telemetry read model is unavailable")
		return uuid.Nil, false
	}
	id, err := parseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-node-id", "Invalid node ID", "node_id must be a UUIDv7")
		return uuid.Nil, false
	}
	return id, true
}
