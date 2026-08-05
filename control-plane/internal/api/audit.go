package api

import (
	"encoding/hex"
	"net/http"
	"strconv"
	"time"

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
	rows, err := s.pool.Query(r.Context(), `SELECT id,occurred_at,actor_type,actor_id,action,resource_type,resource_id,node_id,request_id,trace_id,command_id,approval_id,result,reason,error_type,previous_event_hash,event_hash FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT $2`, workspace(r), limit)
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
		var resourceID, nodeID, commandID, approvalID *uuid.UUID
		var traceID, reason, errorType *string
		var previous, hash []byte
		if err := rows.Scan(&id, &occurred, &actorType, &actorID, &action, &resourceType, &resourceID, &nodeID, &requestID, &traceID, &commandID, &approvalID, &result, &reason, &errorType, &previous, &hash); err != nil {
			writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Audit unavailable", "audit records are temporarily unavailable")
			return
		}
		items = append(items, map[string]any{"id": id, "occurred_at": occurred, "actor_type": actorType, "actor_id": actorID, "action": action, "resource_type": resourceType, "resource_id": resourceID, "node_id": nodeID, "request_id": requestID, "trace_id": traceID, "command_id": commandID, "approval_id": approvalID, "result": result, "reason": reason, "error_type": errorType, "previous_event_hash": hex.EncodeToString(previous), "event_hash": hex.EncodeToString(hash)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) verifyAudit(w http.ResponseWriter, r *http.Request) {
	if s.audit == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Audit unavailable", "audit verification is unavailable")
		return
	}
	result, err := s.audit.Verify(r.Context(), workspace(r))
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
