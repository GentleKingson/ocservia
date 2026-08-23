package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type syntheticCommandRequest struct {
	Kind             operationstore.SyntheticKind `json:"kind"`
	Message          string                       `json:"message,omitempty"`
	ExpectedVersion  *int64                       `json:"expected_version,omitempty"`
	TTLSeconds       *int64                       `json:"ttl_seconds,omitempty"`
	SupersedePending bool                         `json:"supersede_pending,omitempty"`
}

type controlledCommandRequest struct {
	BootID          string `json:"boot_id,omitempty"`
	ExpectedVersion *int64 `json:"expected_version,omitempty"`
	TTLSeconds      *int64 `json:"ttl_seconds,omitempty"`
	Reason          string `json:"reason"`
}

func (s *Server) sessionAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("session_action")
	switch {
	case strings.HasSuffix(action, ":disconnect"):
		s.createControlledCommand(w, r, operationstore.SessionDisconnect, "session.disconnect", strings.TrimSuffix(action, ":disconnect"), "")
	case strings.HasSuffix(action, ":terminate"):
		s.createControlledCommand(w, r, operationstore.SessionTerminate, "session.terminate", strings.TrimSuffix(action, ":terminate"), "")
	default:
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
	}
}

func (s *Server) ipBanAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("ip_action")
	if !strings.HasSuffix(action, ":remove") {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	s.createControlledCommand(w, r, operationstore.IPBanRemove, "ip_ban.remove", "", strings.TrimSuffix(action, ":remove"))
}

func (s *Server) reloadService(w http.ResponseWriter, r *http.Request) {
	s.createControlledCommand(w, r, operationstore.ServiceReload, "service.reload", "", "")
}

func (s *Server) createControlledCommand(w http.ResponseWriter, r *http.Request, kind operationstore.SyntheticKind, action, sessionID, ip string) {
	if s.operations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "operation service is unavailable")
		return
	}
	nodeID, err := uuid.Parse(r.PathValue("node_id"))
	if err != nil || nodeID.Version() != 7 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "node_id must be a UUIDv7")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/idempotency-key-required", "Idempotency key is required", "Idempotency-Key must be provided")
		return
	}
	var body *controlledCommandRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	if body == nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the controlled operation request must be a JSON object")
		return
	}
	expectedVersion, ok := expectedRevision(r.Header.Get("If-Match"), body.ExpectedVersion)
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/expected-version-required", "Expected version is invalid", "provide If-Match revision-N or expected_version")
		return
	}
	ttl := int64(60)
	if body.TTLSeconds != nil {
		ttl = *body.TTLSeconds
	}
	if ttl < 1 || ttl > 300 || strings.TrimSpace(body.Reason) == "" {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "reason is required and ttl_seconds must be between 1 and 300")
		return
	}
	operation, replayed, err := s.operations.CreateSynthetic(r.Context(), operationstore.CreateRequest{
		NodeID: nodeID, IdempotencyKey: idempotencyKey, ExpectedVersion: expectedVersion,
		Kind: kind, SessionID: sessionID, BootID: body.BootID, IP: ip,
		TTL: time.Duration(ttl) * time.Second, RequestID: requestID(r), Traceparent: requestTraceparent(r),
		ActorID: actorID(r), ActorIdentityID: principal(r).IdentityID, ActorSessionID: principal(r).SessionID, ApprovalID: approvalID(r), Action: action, Reason: strings.TrimSpace(body.Reason),
	})
	if err != nil {
		s.writeOperationError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
	w.Header().Set("ETag", fmt.Sprintf("\"revision-%d\"", operation.Version))
	writeJSON(w, http.StatusAccepted, operation)
}

func actorID(r *http.Request) string {
	actor := principal(r)
	if actor.IdentityID != uuid.Nil {
		return actor.IdentityID.String()
	}
	return "developer"
}

func approvalID(r *http.Request) uuid.UUID {
	value, err := uuid.Parse(strings.TrimSpace(r.Header.Get("X-Approval-ID")))
	if err != nil || value.Version() != 7 {
		return uuid.Nil
	}
	return value
}

func (s *Server) createSyntheticCommand(w http.ResponseWriter, r *http.Request) {
	if s.operations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "operation service is unavailable")
		return
	}
	nodeID, err := uuid.Parse(r.PathValue("node_id"))
	if err != nil || nodeID.Version() != 7 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "node_id must be a UUIDv7")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/idempotency-key-required", "Idempotency key is required", "Idempotency-Key must be provided")
		return
	}
	var body *syntheticCommandRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	if body == nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the synthetic command request must be a JSON object")
		return
	}
	expectedVersion, ok := expectedRevision(r.Header.Get("If-Match"), body.ExpectedVersion)
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/expected-version-required", "Expected version is invalid", "provide If-Match revision-N or expected_version")
		return
	}
	ttl := int64(60)
	if body.TTLSeconds != nil {
		ttl = *body.TTLSeconds
	}
	if ttl < 1 || ttl > 86400 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "ttl_seconds must be between 1 and 86400")
		return
	}
	requestID, _ := r.Context().Value(requestIDKey{}).(string)
	actor := principal(r)
	operation, replayed, err := s.operations.CreateSynthetic(r.Context(), operationstore.CreateRequest{
		NodeID: nodeID, IdempotencyKey: idempotencyKey, ExpectedVersion: expectedVersion,
		Kind: body.Kind, Message: body.Message, SupersedePending: body.SupersedePending,
		TTL: time.Duration(ttl) * time.Second, RequestID: requestID, Traceparent: requestTraceparent(r),
		ActorID: actorID(r), ActorIdentityID: actor.IdentityID, ActorSessionID: actor.SessionID,
		Action: "operation.create", Reason: "operator synthetic command",
	})
	if err != nil {
		s.writeOperationError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
	w.Header().Set("ETag", fmt.Sprintf("\"revision-%d\"", operation.Version))
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) streamOperationEvents(w http.ResponseWriter, r *http.Request) {
	if s.operations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "operation service is unavailable")
		return
	}
	operationID, err := uuid.Parse(r.PathValue("operation_id"))
	if err != nil || operationID.Version() != 7 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "operation_id must be a UUIDv7")
		return
	}
	after, valid := eventStreamCursor(r)
	if !valid {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-cursor", "Cursor is invalid", "Last-Event-ID or after must be a UUIDv7")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, r, http.StatusInternalServerError, "https://ocservia.dev/problems/stream-unavailable", "Stream is unavailable", "streaming is not supported")
		return
	}
	s.serveEventStream(w, r, flusher, true, operationID.String(), "operation:"+operationID.String(), after)
}

func (s *Server) queueMetrics(w http.ResponseWriter, r *http.Request) {
	if s.operations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "operation service is unavailable")
		return
	}
	metrics, err := s.operations.Metrics(r.Context())
	if err != nil {
		s.writeOperationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}

func (s *Server) writeOperationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, operationstore.ErrInvalidRequest):
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the operation request is invalid")
	case errors.Is(err, operationstore.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/idempotency-conflict", "Idempotency conflict", "the Idempotency-Key was already used with different input")
	case errors.Is(err, operationstore.ErrStaleRevision):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/stale-revision", "Resource revision is stale", "the node changed after this operation was prepared")
	case errors.Is(err, operationstore.ErrCapabilityMissing):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/capability-unavailable", "Capability is unavailable", "the node has not advertised and received approval for this operation")
	case errors.Is(err, operationstore.ErrTargetNotObserved):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/target-not-observed", "Target is not observed", "the typed target is not present in the node's current observed state")
	case errors.Is(err, operationstore.ErrBacklogExceeded):
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/command-backlog-exceeded", "Command backlog is full", "the node or workspace remote command backlog has reached its bound")
	case errors.Is(err, approvals.ErrNotReady):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/approval-required", "Approval required", "a matching unexpired approval from a different principal is required")
	case errors.Is(err, operationstore.ErrNodeUnavailable), errors.Is(err, pgx.ErrNoRows):
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested node or operation does not exist")
	default:
		s.logger.ErrorContext(r.Context(), "operation request failed", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "operation state is temporarily unavailable")
	}
}

func expectedRevision(ifMatch string, explicit *int64) (int64, bool) {
	var header int64
	if ifMatch != "" {
		if len(ifMatch) < 3 || ifMatch[0] != '"' || ifMatch[len(ifMatch)-1] != '"' {
			return 0, false
		}
		value := ifMatch[1 : len(ifMatch)-1]
		if strings.ContainsRune(value, '"') {
			return 0, false
		}
		if !strings.HasPrefix(value, "revision-") {
			return 0, false
		}
		parsed, err := strconv.ParseInt(strings.TrimPrefix(value, "revision-"), 10, 64)
		if err != nil || parsed < 1 {
			return 0, false
		}
		header = parsed
	}
	if explicit != nil && *explicit < 1 {
		return 0, false
	}
	if explicit != nil && header != 0 && *explicit != header {
		return 0, false
	}
	if explicit != nil {
		return *explicit, true
	}
	return header, header > 0
}
