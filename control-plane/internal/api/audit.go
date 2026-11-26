package api

import (
	"encoding/base64"
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
	if s.backend == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Audit unavailable", "audit records are temporarily unavailable")
		return
	}
	items, err := auditstore.Of(s.backend).RecentEvents(r.Context(), workspace(r), limit)
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Audit unavailable", "audit records are temporarily unavailable")
		return
	}
	if items == nil {
		items = []auditstore.RecentEvent{}
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
	if s.backend == nil {
		unavailable()
		return
	}
	records, err := auditstore.Of(s.backend).VpnAuditRecords(r.Context(), q)
	if err != nil {
		unavailable()
		return
	}
	items := records
	if items == nil {
		items = []auditstore.VpnAuditRecord{}
	}
	var next string
	if len(records) > q.Limit {
		items = records[:q.Limit]
		last := items[len(items)-1]
		at := last.LastSeenAt
		key := last.SessionID
		if last.EventID != nil {
			key = last.EventID.String()
		} else if last.ConnectedAt != nil {
			at = *last.ConnectedAt
		} else {
			unavailable()
			return
		}
		b, err := json.Marshal(auditstore.VpnAuditCursor{At: at.UTC(), Node: last.NodeID, Key: key, Kind: q.Kind})
		if err != nil {
			unavailable()
			return
		}
		next = base64.RawURLEncoding.EncodeToString(b)
	}
	page := map[string]any{"items": items}
	if next != "" {
		page["next_cursor"] = next
	}
	writeJSON(w, 200, page)
}
