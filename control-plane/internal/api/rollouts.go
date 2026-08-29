package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	approvalstore "github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type agentRolloutRequest struct {
	TargetVersion string   `json:"target_version"`
	NodeIDs       []string `json:"node_ids"`
	BatchSize     *int     `json:"batch_size,omitempty"`
	Reason        string   `json:"reason"`
	ApprovalID    string   `json:"approval_id"`
}

// createAgentRollout records a durable fleet rollout. The browser only
// names the target version and the candidate nodes: the server canonicalizes
// the node set, recomputes eligibility from durable evidence, resolves
// package digests from the trusted release catalog, and consumes one
// approval bound to the exact immutable rollout request.
func (s *Server) createAgentRollout(w http.ResponseWriter, r *http.Request) {
	if s.operations == nil || s.releaseCatalog == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "operation service is unavailable")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/idempotency-key-required", "Idempotency key is required", "Idempotency-Key must be provided")
		return
	}
	var body *agentRolloutRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	if body == nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the agent rollout request must be a JSON object")
		return
	}
	approval, approvalErr := uuid.Parse(strings.TrimSpace(body.ApprovalID))
	if approvalErr != nil || approval.Version() != 7 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "approval_id must be a UUIDv7")
		return
	}
	nodeIDs := make([]uuid.UUID, 0, len(body.NodeIDs))
	for _, raw := range body.NodeIDs {
		nodeID, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil || nodeID.Version() != 7 {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "every node_id must be a UUIDv7")
			return
		}
		nodeIDs = append(nodeIDs, nodeID)
	}
	batchSize := 5
	if body.BatchSize != nil {
		batchSize = *body.BatchSize
	}
	actor := principal(r)
	rollout, replayed, err := s.operations.CreateAgentRollout(r.Context(), operationstore.CreateAgentRolloutRequest{
		WorkspaceID:     workspace(r),
		TargetVersion:   strings.TrimSpace(body.TargetVersion),
		NodeIDs:         nodeIDs,
		BatchSize:       batchSize,
		StopOnFailure:   true,
		Reason:          strings.TrimSpace(body.Reason),
		ApprovalID:      approval,
		ActorID:         actorID(r),
		ActorIdentityID: actor.IdentityID,
		ActorSessionID:  actor.SessionID,
		IdempotencyKey:  idempotencyKey,
		RequestID:       requestID(r),
		Traceparent:     requestTraceparent(r),
	})
	if err != nil {
		s.writeRolloutError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/api/v1/agent-rollouts/"+rollout.ID)
	writeJSON(w, http.StatusCreated, rollout)
}

func (s *Server) listAgentRollouts(w http.ResponseWriter, r *http.Request) {
	if s.operations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "operation service is unavailable")
		return
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "limit must be between 1 and 100")
			return
		}
		limit = parsed
	}
	rollouts, err := s.operations.ListAgentRollouts(r.Context(), workspace(r), limit)
	if err != nil {
		s.writeRolloutError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rollouts": rollouts})
}

func (s *Server) getAgentRollout(w http.ResponseWriter, r *http.Request) {
	if s.operations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "operation service is unavailable")
		return
	}
	rolloutID, ok := pathUUIDv7(r.PathValue("rollout_id"))
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "rollout_id must be a UUIDv7")
		return
	}
	rollout, err := s.operations.GetAgentRollout(r.Context(), rolloutID)
	if err != nil {
		s.writeRolloutError(w, r, err)
		return
	}
	if rollout.WorkspaceID != workspace(r).String() {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested rollout does not exist")
		return
	}
	writeJSON(w, http.StatusOK, rollout)
}

// resumeAgentRollout records the explicit operator decision to continue a
// paused rollout. No next batch ever starts automatically after a pause.
func (s *Server) resumeAgentRollout(w http.ResponseWriter, r *http.Request) {
	if s.operations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "operation service is unavailable")
		return
	}
	rolloutID, ok := pathUUIDv7(r.PathValue("rollout_id"))
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "rollout_id must be a UUIDv7")
		return
	}
	rollout, err := s.operations.GetAgentRollout(r.Context(), rolloutID)
	if err != nil {
		s.writeRolloutError(w, r, err)
		return
	}
	if rollout.WorkspaceID != workspace(r).String() {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested rollout does not exist")
		return
	}
	actor := principal(r)
	resumed, err := s.operations.ResumeAgentRollout(r.Context(), rolloutID, actorID(r), actor.IdentityID, actor.SessionID, requestID(r), requestTraceparent(r))
	if err != nil {
		s.writeRolloutError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resumed)
}

func (s *Server) writeRolloutError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, operationstore.ErrRolloutInvalid):
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the agent rollout request is invalid")
	case errors.Is(err, operationstore.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/idempotency-conflict", "Idempotency conflict", "the Idempotency-Key was already used with different input")
	case errors.Is(err, operationstore.ErrNoEligibleNodes):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/no-eligible-nodes", "No eligible nodes", "none of the requested nodes is currently eligible for the target version")
	case errors.Is(err, operationstore.ErrRolloutState):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/rollout-state", "Rollout state is invalid", "only a paused rollout can be resumed")
	case errors.Is(err, operationstore.ErrRolloutUnavailable):
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "rollout orchestration is unavailable")
	case errors.Is(err, approvalstore.ErrNotReady):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/approval-required", "Approval required", "the approval is expired, consumed, or does not match the rollout request")
	case errors.Is(err, pgx.ErrNoRows):
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested rollout does not exist")
	default:
		s.logger.ErrorContext(r.Context(), "agent rollout request failed", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "rollout state is temporarily unavailable")
	}
}
