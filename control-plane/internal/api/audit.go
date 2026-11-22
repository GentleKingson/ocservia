package api

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"

	"github.com/google/uuid"
)

func (s *Server) listAuditEvents(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-page-size", "Invalid page size", "page_size must be between 1 and 200")
			return
		}
		limit = parsed
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

func (s *Server) listVpnAuditRecords(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	q := auditstore.VpnAuditFilter{Workspace: workspace(r), Username: query.Get("username"), Limit: 100}
	invalid := func() {
		writeProblem(w, r, 400, "https://ocservia.dev/problems/invalid-filter", "Invalid VPN audit filter", "Check the node, username, time range and page size")
	}
	q.Kind = query.Get("kind")
	if q.Kind == "" {
		q.Kind = "sessions"
	}
	if q.Kind != "sessions" && q.Kind != "observations" && q.Kind != "network" {
		invalid()
		return
	}
	if raw := query.Get("cursor"); raw != "" {
		var c auditstore.VpnAuditCursor
		if len(raw) > 2048 {
			invalid()
			return
		}
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || json.Unmarshal(b, &c) != nil || c.Kind != q.Kind || c.Node == uuid.Nil || c.At.IsZero() || len(c.Key) > 256 || c.Key == "" {
			invalid()
			return
		}
		if q.Kind != "observations" {
			if _, err := uuid.Parse(c.Key); err != nil {
				invalid()
				return
			}
		}
		q.After = &c
	}
	q.Destination = query.Get("destination_ip")
	if q.Destination != "" {
		addr, err := netip.ParseAddr(q.Destination)
		if q.Kind != "network" || err != nil || addr.Zone() != "" {
			invalid()
			return
		}
	}
	if len(q.Username) > 128 {
		invalid()
		return
	}
	if raw := query.Get("node_id"); raw != "" {
		node, err := uuid.Parse(raw)
		if err != nil {
			invalid()
			return
		}
		q.Node = &node
	}
	for key, target := range map[string]**time.Time{"since": &q.Since, "until": &q.Until} {
		if raw := query.Get(key); raw != "" {
			at, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				invalid()
				return
			}
			*target = &at
		}
	}
	if q.Since != nil && q.Until != nil && !q.Since.Before(*q.Until) {
		invalid()
		return
	}
	if raw := query.Get("page_size"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			invalid()
			return
		}
		q.Limit = n
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
	items := []map[string]any{}
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
		var node uuid.UUID
		var session, user, client, kind string
		var vpn, rx, tx, dest, protocol *string
		var event *uuid.UUID
		var port *int32
		var last time.Time
		var connected, absent, started *time.Time
		if err := rows.Scan(&node, &session, &user, &client, &vpn, &connected, &rx, &tx, &last, &absent, &kind, &event, &dest, &port, &protocol, &started); err != nil {
			unavailable()
			return
		}
		item := map[string]any{"node_id": node, "session_id": session, "username": user, "client_ip": client, "last_seen_at": last, "event_type": kind}
		if rx != nil {
			item["bytes_in"] = *rx
		}
		if tx != nil {
			item["bytes_out"] = *tx
		}
		if connected != nil {
			item["connected_at"] = connected
		}
		if started != nil {
			item["flow_started_at"] = started
		}
		if event != nil {
			item["event_id"] = *event
		}
		if dest != nil {
			item["destination_ip"] = *dest
		}
		if protocol != nil {
			item["protocol"] = *protocol
		}
		if port != nil {
			item["destination_port"] = *port
		}
		if absent != nil {
			item["absent_at"] = absent
		}
		if vpn != nil {
			item["vpn_ip"] = *vpn
		}
		if len(items)+1 == q.Limit {
			at := last
			key := session
			if event != nil {
				key = event.String()
			} else if connected != nil {
				at = *connected
			} else {
				unavailable()
				return
			}
			cursor = auditstore.VpnAuditCursor{At: at.UTC(), Node: node, Key: key, Kind: q.Kind}
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
