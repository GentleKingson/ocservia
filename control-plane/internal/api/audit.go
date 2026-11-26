package api

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"

	"github.com/google/uuid"
)

func (s *Server) listAuditEvents(w http.ResponseWriter, r *http.Request) {
	limit, ok := pageSize(r, 100)
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-page-size", "Invalid page size", "page_size must be between 1 and 200")
		return
	}
	store, err := auditstore.From(s.backend)
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Audit unavailable", "audit records are temporarily unavailable")
		return
	}
	rows, err := store.RecentEvents(r.Context(), workspace(r), limit)
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Audit unavailable", "audit records are temporarily unavailable")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id uuid.UUID
		var occurred time.Time
		var actorType, actorID, action, resourceType, requestID, result string
		var resourceID, nodeID, commandID *uuid.UUID
		var traceID, reason, errorType *string
		var previous, hash []byte
		if err := rows.Scan(&id, &occurred, &actorType, &actorID, &action, &resourceType, &resourceID, &nodeID, &requestID, &traceID, &commandID, &result, &reason, &errorType, &previous, &hash); err != nil {
			writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Audit unavailable", "audit records are temporarily unavailable")
			return
		}
		items = append(items, map[string]any{"id": id, "occurred_at": occurred, "actor_type": actorType, "actor_id": actorID, "action": action, "resource_type": resourceType, "resource_id": resourceID, "node_id": nodeID, "request_id": requestID, "trace_id": traceID, "command_id": commandID, "result": result, "reason": reason, "error_type": errorType, "previous_event_hash": hex.EncodeToString(previous), "event_hash": hex.EncodeToString(hash)})
	}
	if err := rows.Err(); err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Audit unavailable", "audit records are temporarily unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) verifyAudit(w http.ResponseWriter, r *http.Request) {
	if s.audit == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Audit unavailable", "audit verification is unavailable")
		return
	}
	result, err := s.audit.Verify(r.Context(), workspace(r))
	if errors.Is(err, audit.ErrVerificationBusy) {
		w.Header().Set("Retry-After", "1")
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Audit busy", "audit verification is already running")
		return
	}
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Audit unavailable", "audit verification could not complete")
		return
	}
	status := http.StatusOK
	if !result.Valid || !result.Checkpoint {
		status = http.StatusConflict
	}
	writeJSON(w, status, result)
}

type vpnAuditRecord struct {
	NodeID          uuid.UUID  `json:"node_id"`
	SessionID       string     `json:"session_id"`
	Username        string     `json:"username"`
	ClientIP        string     `json:"client_ip"`
	VPNIP           *string    `json:"vpn_ip,omitempty"`
	ConnectedAt     *time.Time `json:"connected_at,omitempty"`
	BytesIn         *string    `json:"bytes_in,omitempty"`
	BytesOut        *string    `json:"bytes_out,omitempty"`
	LastSeenAt      time.Time  `json:"last_seen_at"`
	AbsentAt        *time.Time `json:"absent_at,omitempty"`
	EventType       string     `json:"event_type"`
	EventID         *uuid.UUID `json:"event_id,omitempty"`
	DestinationIP   *string    `json:"destination_ip,omitempty"`
	DestinationPort *int32     `json:"destination_port,omitempty"`
	Protocol        *string    `json:"protocol,omitempty"`
	FlowStartedAt   *time.Time `json:"flow_started_at,omitempty"`
}

func parseVpnAuditFilter(r *http.Request) (auditstore.VpnAuditFilter, bool) {
	query := r.URL.Query()
	q := auditstore.VpnAuditFilter{
		Workspace:   workspace(r),
		Username:    query.Get("username"),
		Destination: query.Get("destination_ip"),
		Kind:        query.Get("kind"),
	}
	if q.Kind == "" {
		q.Kind = "sessions"
	}
	if q.Kind != "sessions" && q.Kind != "observations" && q.Kind != "network" {
		return auditstore.VpnAuditFilter{}, false
	}
	if raw := query.Get("cursor"); raw != "" {
		var c auditstore.VpnAuditCursor
		if len(raw) > 2048 {
			return auditstore.VpnAuditFilter{}, false
		}
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || json.Unmarshal(b, &c) != nil || c.Kind != q.Kind || c.Node == uuid.Nil || c.At.IsZero() || len(c.Key) > 256 || c.Key == "" {
			return auditstore.VpnAuditFilter{}, false
		}
		if q.Kind != "observations" {
			if _, err := uuid.Parse(c.Key); err != nil {
				return auditstore.VpnAuditFilter{}, false
			}
		}
		q.After = &c
	}
	if q.Destination != "" {
		addr, err := netip.ParseAddr(q.Destination)
		if q.Kind != "network" || err != nil || addr.Zone() != "" {
			return auditstore.VpnAuditFilter{}, false
		}
	}
	if len(q.Username) > 128 {
		return auditstore.VpnAuditFilter{}, false
	}
	if raw := query.Get("node_id"); raw != "" {
		node, err := uuid.Parse(raw)
		if err != nil {
			return auditstore.VpnAuditFilter{}, false
		}
		q.Node = &node
	}
	for key, target := range map[string]**time.Time{"since": &q.Since, "until": &q.Until} {
		if raw := query.Get(key); raw != "" {
			at, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return auditstore.VpnAuditFilter{}, false
			}
			*target = &at
		}
	}
	if q.Since != nil && q.Until != nil && !q.Since.Before(*q.Until) {
		return auditstore.VpnAuditFilter{}, false
	}
	limit, ok := pageSize(r, 100)
	if !ok {
		return auditstore.VpnAuditFilter{}, false
	}
	q.Limit = limit
	return q, true
}

func (s *Server) listVpnAuditRecords(w http.ResponseWriter, r *http.Request) {
	q, ok := parseVpnAuditFilter(r)
	if !ok {
		writeProblem(w, r, 400, "https://ocservia.dev/problems/invalid-filter", "Invalid VPN audit filter", "Check the node, username, time range and page size")
		return
	}
	unavailable := func() {
		writeProblem(w, r, 503, "https://ocservia.dev/problems/database-unavailable", "VPN audit unavailable", "VPN audit records could not be read")
	}
	store, err := auditstore.From(s.backend)
	if err != nil {
		unavailable()
		return
	}
	rows, err := store.VpnAuditRecords(r.Context(), q)
	if err != nil {
		unavailable()
		return
	}
	defer rows.Close()
	items := []vpnAuditRecord{}
	var cursor auditstore.VpnAuditCursor
	var next string
	for rows.Next() {
		if len(items) == q.Limit {
			b, err := json.Marshal(cursor)
			if err != nil {
				unavailable()
				return
			}
			next = base64.RawURLEncoding.EncodeToString(b)
			break
		}
		var item vpnAuditRecord
		if err := rows.Scan(&item.NodeID, &item.SessionID, &item.Username, &item.ClientIP, &item.VPNIP, &item.ConnectedAt, &item.BytesIn, &item.BytesOut, &item.LastSeenAt, &item.AbsentAt, &item.EventType, &item.EventID, &item.DestinationIP, &item.DestinationPort, &item.Protocol, &item.FlowStartedAt); err != nil {
			unavailable()
			return
		}
		if len(items)+1 == q.Limit {
			at := item.LastSeenAt
			key := item.SessionID
			if item.EventID != nil {
				key = item.EventID.String()
			} else if item.ConnectedAt != nil {
				at = *item.ConnectedAt
			} else {
				unavailable()
				return
			}
			cursor = auditstore.VpnAuditCursor{At: at.UTC(), Node: item.NodeID, Key: key, Kind: q.Kind}
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		unavailable()
		return
	}
	page := map[string]any{"items": items}
	if next != "" {
		page["next_cursor"] = next
	}
	writeJSON(w, 200, page)
}
