package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type userPolicyRequest struct {
	QuotaPeriod     string  `json:"quota_period"`
	QuotaDirection  string  `json:"quota_direction"`
	QuotaBytes      int64   `json:"quota_bytes"`
	ExpiresAt       *string `json:"expires_at"`
	ExpectedVersion int64   `json:"expected_version"`
	Reason          string  `json:"reason"`
}

type userBatchRequest struct {
	BatchID uuid.UUID                         `json:"batch_id"`
	Reason  string                            `json:"reason"`
	Items   []useroperations.BatchItemRequest `json:"items"`
}

func (s *Server) getUserPolicy(w http.ResponseWriter, r *http.Request) {
	if s.useroperations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "user operations service is unavailable")
		return
	}
	nodeID, ok := pathUUIDv7(r.PathValue("node_id"))
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "node_id must be a UUIDv7")
		return
	}
	policy, err := s.useroperations.GetPolicy(r.Context(), nodeID, r.PathValue("username"))
	if err != nil {
		s.writeUserOperationsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) setUserPolicy(w http.ResponseWriter, r *http.Request) {
	if s.useroperations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "user operations service is unavailable")
		return
	}
	nodeID, ok := pathUUIDv7(r.PathValue("node_id"))
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "node_id must be a UUIDv7")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/idempotency-key-required", "Idempotency key is required", "Idempotency-Key must be provided")
		return
	}
	var body userPolicyRequest
	if !decodeSingleJSON(w, r, &body) {
		return
	}
	var expiresAt *time.Time
	if body.ExpiresAt != nil {
		parsed, err := time.Parse(time.RFC3339, *body.ExpiresAt)
		if err != nil || !strings.HasSuffix(*body.ExpiresAt, "Z") {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "expires_at must be an RFC 3339 UTC timestamp ending in Z")
			return
		}
		parsed = parsed.UTC()
		expiresAt = &parsed
	}
	actor := principal(r)
	policy, replayed, err := s.useroperations.SetPolicy(r.Context(), useroperations.PolicyRequest{NodeID: nodeID, Username: r.PathValue("username"), QuotaPeriod: body.QuotaPeriod, QuotaDirection: body.QuotaDirection, QuotaBytes: body.QuotaBytes, ExpiresAt: expiresAt, ExpectedVersion: body.ExpectedVersion, IdempotencyKey: idempotencyKey, ActorID: actorID(r), ActorIdentityID: actor.IdentityID, ActorSessionID: actor.SessionID, Reason: strings.TrimSpace(body.Reason), RequestID: requestID(r), Traceparent: requestTraceparent(r)})
	if err != nil {
		s.writeUserOperationsError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("ETag", `"revision-`+strconv.FormatInt(policy.Version, 10)+`"`)
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) createUserBatch(w http.ResponseWriter, r *http.Request) {
	if s.useroperations == nil || s.rbac == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "user operations service is unavailable")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/idempotency-key-required", "Idempotency key is required", "Idempotency-Key must be provided")
		return
	}
	var body userBatchRequest
	if !decodeSingleJSON(w, r, &body) {
		return
	}
	principal := principal(r)
	workspaceID := workspace(r)
	for index := range body.Items {
		resource, err := s.rbac.Node(r.Context(), body.Items[index].NodeID)
		if err != nil || resource.WorkspaceID != workspaceID {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "every batch item must reference an existing user in the selected workspace")
			return
		}
		body.Items[index].Authorized = principal.Issuer == "development" || s.rbac.Authorize(r.Context(), principal.IdentityID, "user.manage", resource, principal.BreakGlass) == nil
	}
	batch, replayed, err := s.useroperations.CreateBatch(r.Context(), useroperations.BatchRequest{ID: body.BatchID, WorkspaceID: workspaceID, ActorIdentityID: principal.IdentityID, ActorSessionID: principal.SessionID, ApprovalID: approvalID(r), ActorID: actorID(r), Reason: strings.TrimSpace(body.Reason), RequestID: requestID(r), Traceparent: requestTraceparent(r), IdempotencyKey: idempotencyKey, Items: body.Items})
	if err != nil {
		s.writeUserOperationsError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/api/v1/user-batches/"+batch.ID.String())
	writeJSON(w, http.StatusAccepted, batch)
}

func (s *Server) getUserBatch(w http.ResponseWriter, r *http.Request) {
	if s.useroperations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "user operations service is unavailable")
		return
	}
	id, err := uuid.Parse(r.PathValue("batch_id"))
	if err != nil || id.Version() != 7 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "batch_id must be a UUIDv7")
		return
	}
	batch, err := s.useroperations.GetBatch(r.Context(), id)
	if err != nil || batch.WorkspaceID != workspace(r) {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested batch does not exist")
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

func decodeSingleJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the request body is invalid")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the request must contain one JSON object")
		return false
	}
	return true
}

func (s *Server) writeUserOperationsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, useroperations.ErrInvalidRequest):
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the quota, expiry, or batch request failed validation")
	case errors.Is(err, useroperations.ErrVersionConflict):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/stale-revision", "Resource revision is stale", "the user policy changed after this request was prepared")
	case errors.Is(err, useroperations.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/idempotency-conflict", "Idempotency conflict", "the Idempotency-Key was already used with different input")
	case errors.Is(err, useroperations.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested user policy or batch does not exist")
	case errors.Is(err, rbac.ErrForbidden):
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Access denied", "the principal is not authorized for this operation")
	case errors.Is(err, approvals.ErrNotReady):
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/approval-required", "Independent approval is required", "bulk user disable requires a valid approval bound to the batch identifier")
	default:
		s.logger.ErrorContext(r.Context(), "user operations request failed", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "user operations state is temporarily unavailable")
	}
}
