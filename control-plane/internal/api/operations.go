package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
)

type syntheticCommandRequest struct {
	Kind             operations.Kind `json:"kind"`
	Message          string          `json:"message,omitempty"`
	ExpectedVersion  *int64          `json:"expected_version,omitempty"`
	TTLSeconds       *int64          `json:"ttl_seconds,omitempty"`
	SupersedePending bool            `json:"supersede_pending,omitempty"`
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
		s.createControlledCommand(w, r, operations.SessionDisconnect, "session.disconnect", strings.TrimSuffix(action, ":disconnect"), "")
	case strings.HasSuffix(action, ":terminate"):
		s.createControlledCommand(w, r, operations.SessionTerminate, "session.terminate", strings.TrimSuffix(action, ":terminate"), "")
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
	s.createControlledCommand(w, r, operations.IPBanRemove, "ip_ban.remove", "", strings.TrimSuffix(action, ":remove"))
}

func (s *Server) reloadService(w http.ResponseWriter, r *http.Request) {
	s.createControlledCommand(w, r, operations.ServiceReload, "service.reload", "", "")
}

type agentUpgradeRequest struct {
	TargetVersion   string `json:"target_version"`
	Reason          string `json:"reason"`
	ExpectedVersion *int64 `json:"expected_version,omitempty"`
	TTLSeconds      *int64 `json:"ttl_seconds,omitempty"`
}

// upgradeAgent creates the single-node agent upgrade operation. The browser
// only selects a version: the package digest always resolves from the
// operator-provisioned trusted release catalog for the node's observed
// architecture, so no URL, path, or caller-supplied digest exists.
func (s *Server) upgradeAgent(w http.ResponseWriter, r *http.Request) {
	if s.operations == nil || s.releaseCatalog == nil {
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
	var body *agentUpgradeRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	if body == nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the agent upgrade request must be a JSON object")
		return
	}
	expectedVersion, ok := expectedRevision(r.Header.Get("If-Match"), body.ExpectedVersion)
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/expected-version-required", "Expected version is invalid", "provide If-Match revision-N or expected_version")
		return
	}
	ttl := int64(300)
	if body.TTLSeconds != nil {
		ttl = *body.TTLSeconds
	}
	reason := strings.TrimSpace(body.Reason)
	if ttl < 60 || ttl > 3600 || reason == "" || len(reason) > 512 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "target_version and reason are required and ttl_seconds must be between 60 and 3600")
		return
	}
	target, err := s.operations.PrepareAgentUpgrade(r.Context(), workspace(r), nodeID, body.TargetVersion)
	if err != nil {
		switch {
		case errors.Is(err, operations.ErrInvalidRequest):
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "target_version and reason are required and ttl_seconds must be between 60 and 3600")
		case errors.Is(err, operations.ErrUpgradeArchitectureUnknown):
			writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/release-not-trusted", "Release is not trusted", "the node has not reported its package architecture yet")
		case errors.Is(err, operations.ErrUpgradeReleaseNotTrusted):
			writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/release-not-trusted", "Release is not trusted", "no trusted release exists for the requested version and architecture")
		default:
			writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested node does not exist")
		}
		return
	}
	operation, replayed, err := s.operations.Create(r.Context(), operations.CreateRequest{
		NodeID: nodeID, IdempotencyKey: idempotencyKey, ExpectedVersion: expectedVersion,
		Kind: operations.AgentUpgrade, TargetVersion: target.Version, PackageSHA256: target.PackageSHA256[:], Architecture: target.Architecture,
		Action: "agent.upgrade", Reason: reason,
		TTL: time.Duration(ttl) * time.Second, RequestID: requestID(r), Traceparent: requestTraceparent(r),
		ActorID: actorID(r), ActorIdentityID: principal(r).IdentityID, ActorSessionID: principal(r).SessionID,
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

func (s *Server) createControlledCommand(w http.ResponseWriter, r *http.Request, kind operations.Kind, action, sessionID, ip string) {
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
	operation, replayed, err := s.operations.Create(r.Context(), operations.CreateRequest{
		NodeID: nodeID, IdempotencyKey: idempotencyKey, ExpectedVersion: expectedVersion,
		Kind: kind, SessionID: sessionID, BootID: body.BootID, IP: ip,
		TTL: time.Duration(ttl) * time.Second, RequestID: requestID(r), Traceparent: requestTraceparent(r),
		ActorID: actorID(r), ActorIdentityID: principal(r).IdentityID, ActorSessionID: principal(r).SessionID, Action: action, Reason: strings.TrimSpace(body.Reason),
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
	operation, replayed, err := s.operations.Create(r.Context(), operations.CreateRequest{
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
	// The unscoped development principal keeps controller-wide totals; every
	// other principal only sees the workspace it was authorized for.
	var metrics operations.QueueMetrics
	var err error
	if principal(r).Issuer == "development" {
		metrics, err = s.operations.Metrics(r.Context())
	} else {
		metrics, err = s.operations.WorkspaceMetrics(r.Context(), workspace(r))
	}
	if err != nil {
		s.writeOperationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}

func (s *Server) writeOperationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrForbidden):
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Access denied", "the session or resource authority is no longer valid")
	case errors.Is(err, operations.ErrInvalidRequest):
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the operation request is invalid")
	case errors.Is(err, operations.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/idempotency-conflict", "Idempotency conflict", "the Idempotency-Key was already used with different input")
	case errors.Is(err, operations.ErrStaleRevision):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/stale-revision", "Resource revision is stale", "the node changed after this operation was prepared")
	case errors.Is(err, operations.ErrCapabilityMissing):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/capability-unavailable", "Capability is unavailable", "the node has not advertised and received approval for this operation")
	case errors.Is(err, operations.ErrTargetNotObserved):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/target-not-observed", "Target is not observed", "the typed target is not present in the node's current observed state")
	case errors.Is(err, operations.ErrBacklogExceeded):
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/command-backlog-exceeded", "Command backlog is full", "the node or workspace remote command backlog has reached its bound")
	case errors.Is(err, operations.ErrUpgradeActive):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/upgrade-already-active", "Upgrade is already active", "another agent upgrade is already active for this node")
	case errors.Is(err, operations.ErrNodeUnavailable), errors.Is(err, database.ErrNotFound):
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

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	service := s.operations
	if service == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	id, err := uuid.Parse(r.PathValue("operation_id"))
	if err != nil || id.Version() != 7 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "operation_id must be a UUIDv7")
		return
	}
	operation, err := service.Get(r.Context(), id)
	if errors.Is(err, database.ErrNotFound) {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested operation does not exist")
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "get operation", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "operation state is temporarily unavailable")
		return
	}
	w.Header().Set("ETag", fmt.Sprintf("\"revision-%d\"", operation.Version))
	writeJSON(w, http.StatusOK, operation)
}

func (s *Server) listOperations(w http.ResponseWriter, r *http.Request) {
	service := s.operations
	if service == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	after, ok := parseOptionalUUIDv7(r.URL.Query().Get("cursor"))
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-cursor", "Cursor is invalid", "cursor must be a UUIDv7 operation ID")
		return
	}
	limit, ok := pageSize(r, 50)
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-page-size", "Page size is invalid", "page_size must be an integer between 1 and 200")
		return
	}
	operations, hasMore, err := service.ListInWorkspace(r.Context(), workspace(r), after, limit)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "list operations", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "operations are temporarily unavailable")
		return
	}
	page := map[string]any{"has_more": hasMore}
	if hasMore && len(operations) > 0 {
		page["next_cursor"] = operations[len(operations)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": operations, "page": page})
}

func (s *Server) operationSummary(w http.ResponseWriter, r *http.Request) {
	service := s.operations
	if service == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	summary, err := service.SummaryInWorkspace(r.Context(), workspace(r))
	if err != nil {
		s.logger.ErrorContext(r.Context(), "operation summary", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "operations are temporarily unavailable")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
