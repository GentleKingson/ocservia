package nodehttp

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/GentleKingson/ocservia/control-plane/internal/api/httpx"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/google/uuid"
)

// Handler owns the node read routes, not authentication or service lifecycle.
type Handler struct {
	reader    Reader
	logger    *slog.Logger
	workspace func(*http.Request) uuid.UUID
}

// New requires access to the workspace already selected by the authorization guard.
func New(reader Reader, logger *slog.Logger, workspace func(*http.Request) uuid.UUID) *Handler {
	if workspace == nil {
		panic("nodehttp: authorized workspace accessor is required")
	}
	return &Handler{reader: reader, logger: logger, workspace: workspace}
}

func (h *Handler) listNodes(w http.ResponseWriter, r *http.Request) {
	if h.reader == nil {
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/telemetry-unavailable", "Telemetry unavailable", "the telemetry read model is unavailable")
		return
	}
	after, ok := httpx.ParseOptionalUUIDv7(r.URL.Query().Get("cursor"))
	if !ok {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-cursor", "Invalid cursor", "cursor must be a UUIDv7 node ID")
		return
	}
	limit, ok := httpx.PageSize(r, 50)
	if !ok {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-page-size", "Invalid page size", "page_size must be between 1 and 200")
		return
	}
	nodes, hasMore, err := h.reader.ListNodesInWorkspace(r.Context(), h.workspace(r), after, limit)
	if err != nil {
		h.logger.Error("list nodes failed", "error", err)
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Nodes unavailable", "node state could not be read")
		return
	}
	page := map[string]any{"has_more": hasMore}
	if hasMore && len(nodes) > 0 {
		page["next_cursor"] = nodes[len(nodes)-1].ID
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": nodes, "page": page})
}

func (h *Handler) getNode(w http.ResponseWriter, r *http.Request) {
	id, ok := h.nodePathID(w, r)
	if !ok {
		return
	}
	node, err := h.reader.GetNode(r.Context(), id)
	if errors.Is(err, database.ErrNotFound) {
		httpx.WriteProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Node not found", "the requested node does not exist")
		return
	}
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Node unavailable", "node state could not be read")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, node)
}

func (h *Handler) listNodeSessions(w http.ResponseWriter, r *http.Request) {
	id, ok := h.nodePathID(w, r)
	if !ok {
		return
	}
	cursor := r.URL.Query().Get("cursor")
	limit, valid := httpx.PageSize(r, 50)
	if !valid || len(cursor) > 256 {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-pagination", "Invalid pagination", "cursor and page_size are outside permitted bounds")
		return
	}
	items, hasMore, err := h.reader.ListSessions(r.Context(), id, cursor, limit)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Sessions unavailable", "session state could not be read")
		return
	}
	page := map[string]any{"has_more": hasMore}
	if hasMore && len(items) > 0 {
		page["next_cursor"] = items[len(items)-1].ID
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "page": page})
}

func (h *Handler) listNodeIPBans(w http.ResponseWriter, r *http.Request) {
	id, ok := h.nodePathID(w, r)
	if !ok {
		return
	}
	items, err := h.reader.ListIPBans(r.Context(), id, 200)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "IP bans unavailable", "IP ban state could not be read")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) listNodeTelemetry(w http.ResponseWriter, r *http.Request) {
	id, ok := h.nodePathID(w, r)
	if !ok {
		return
	}
	metric := r.URL.Query().Get("metric")
	resolution := r.URL.Query().Get("resolution")
	if resolution == "" {
		resolution = "5m"
	}
	since := value.Timestamp{}
	if text := r.URL.Query().Get("since"); text != "" {
		var err error
		since, err = value.ParseTimestamp(text)
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-query", "Invalid query", "since must be an RFC 3339 timestamp, signed six-digit extended-year timestamp, infinity or -infinity")
			return
		}
	}
	items, err := h.reader.HistoryFrom(r.Context(), id, metric, resolution, since)
	if err != nil {
		switch {
		case errors.Is(err, telemetry.ErrInvalidMetric):
			httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-query", "Invalid query", "metric is invalid")
		case errors.Is(err, telemetry.ErrInvalidResolution):
			httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-query", "Invalid query", "resolution is invalid")
		default:
			h.logger.ErrorContext(r.Context(), "read telemetry history failed", "error", err)
			httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Telemetry unavailable", "telemetry history could not be read")
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "resolution": resolution})
}

func (h *Handler) nodePathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	if h.reader == nil {
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/telemetry-unavailable", "Telemetry unavailable", "the telemetry read model is unavailable")
		return uuid.Nil, false
	}
	id, err := uuid.Parse(r.PathValue("node_id"))
	if err != nil || id.Version() != 7 {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-node-id", "Invalid node ID", "node_id must be a UUIDv7")
		return uuid.Nil, false
	}
	return id, true
}
