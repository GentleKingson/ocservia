package useroperationshttp

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/api/httpx"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
)

// Handler owns policy and batch HTTP behavior, not service lifecycle or scheduling.
type Handler struct {
	operations  Operations
	authorizer  Authorizer
	requestInfo func(*http.Request) RequestInfo
	logger      *slog.Logger
}

func New(operations Operations, authorizer Authorizer, requestInfo func(*http.Request) RequestInfo, logger *slog.Logger) *Handler {
	if requestInfo == nil {
		panic("useroperationshttp: authenticated request accessor is required")
	}
	return &Handler{operations: operations, authorizer: authorizer, logger: logger, requestInfo: requestInfo}
}

type userPolicyRequest struct {
	QuotaPeriod     string  `json:"quota_period"`
	QuotaDirection  string  `json:"quota_direction"`
	QuotaBytes      int64   `json:"quota_bytes"`
	ExpiresAt       *string `json:"expires_at"`
	ExpectedVersion int64   `json:"expected_version"`
	Reason          string  `json:"reason"`
}

type userBatchRequest struct {
	Reason string                            `json:"reason"`
	Items  []useroperations.BatchItemRequest `json:"items"`
}

func (h *Handler) getUserPolicy(w http.ResponseWriter, r *http.Request) {
	if h.operations == nil {
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "user operations service is unavailable")
		return
	}
	nodeID, err := httpx.ParseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "node_id must be a UUIDv7")
		return
	}
	policy, err := h.operations.GetPolicy(r.Context(), nodeID, r.PathValue("username"))
	if err != nil {
		h.writeUserOperationsError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, policy)
}

func (h *Handler) setUserPolicy(w http.ResponseWriter, r *http.Request) {
	if h.operations == nil {
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "user operations service is unavailable")
		return
	}
	nodeID, err := httpx.ParseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "node_id must be a UUIDv7")
		return
	}
	idempotencyKey, ok := httpx.RequireIdempotencyKey(w, r)
	if !ok {
		return
	}
	var body userPolicyRequest
	if !httpx.DecodeStrictJSON(w, r, &body) {
		return
	}
	var expiresAt *time.Time
	if body.ExpiresAt != nil {
		parsed, err := time.Parse(time.RFC3339, *body.ExpiresAt)
		if err != nil || !strings.HasSuffix(*body.ExpiresAt, "Z") {
			httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "expires_at must be an RFC 3339 UTC timestamp ending in Z")
			return
		}
		parsed = parsed.UTC()
		expiresAt = &parsed
	}
	info := h.requestInfo(r)
	actor := info.Principal
	policy, replayed, err := h.operations.SetPolicy(r.Context(), useroperations.PolicyRequest{NodeID: nodeID, Username: r.PathValue("username"), QuotaPeriod: body.QuotaPeriod, QuotaDirection: body.QuotaDirection, QuotaBytes: body.QuotaBytes, ExpiresAt: expiresAt, ExpectedVersion: body.ExpectedVersion, IdempotencyKey: idempotencyKey, ActorID: info.ActorID, ActorIdentityID: actor.IdentityID, ActorSessionID: actor.SessionID, Reason: strings.TrimSpace(body.Reason), RequestID: info.RequestID, Traceparent: info.Traceparent})
	if err != nil {
		h.writeUserOperationsError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("ETag", `"revision-`+strconv.FormatInt(policy.Version, 10)+`"`)
	httpx.WriteJSON(w, http.StatusOK, policy)
}

func (h *Handler) createUserBatch(w http.ResponseWriter, r *http.Request) {
	if h.operations == nil || h.authorizer == nil {
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "user operations service is unavailable")
		return
	}
	idempotencyKey, ok := httpx.RequireIdempotencyKey(w, r)
	if !ok {
		return
	}
	var body userBatchRequest
	if !httpx.DecodeStrictJSON(w, r, &body) {
		return
	}
	info := h.requestInfo(r)
	principal := info.Principal
	workspaceID := info.WorkspaceID
	for index := range body.Items {
		resource, err := h.authorizer.Node(r.Context(), body.Items[index].NodeID)
		if err != nil || resource.WorkspaceID != workspaceID {
			httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "every batch item must reference an existing user in the selected workspace")
			return
		}
		body.Items[index].Authorized = principal.Issuer == "development" || h.authorizer.Authorize(r.Context(), principal.IdentityID, "user.manage", resource, principal.BreakGlass) == nil
	}
	batch, replayed, err := h.operations.CreateBatch(r.Context(), useroperations.BatchRequest{WorkspaceID: workspaceID, ActorIdentityID: principal.IdentityID, ActorSessionID: principal.SessionID, ApprovalID: info.ApprovalID, ActorID: info.ActorID, Reason: strings.TrimSpace(body.Reason), RequestID: info.RequestID, Traceparent: info.Traceparent, IdempotencyKey: idempotencyKey, Items: body.Items})
	if err != nil {
		h.writeUserOperationsError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/api/v1/user-batches/"+batch.ID.String())
	httpx.WriteJSON(w, http.StatusAccepted, batch)
}

func (h *Handler) getUserBatch(w http.ResponseWriter, r *http.Request) {
	if h.operations == nil {
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "user operations service is unavailable")
		return
	}
	id, err := httpx.ParseUUIDv7(r.PathValue("batch_id"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "batch_id must be a UUIDv7")
		return
	}
	batch, err := h.operations.GetBatch(r.Context(), id)
	info := h.requestInfo(r)
	if err != nil || batch.WorkspaceID != info.WorkspaceID {
		httpx.WriteProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested batch does not exist")
		return
	}
	actor := info.Principal
	creator := batch.ActorIdentityID != nil && *batch.ActorIdentityID == actor.IdentityID
	if actor.Issuer != "development" && !creator {
		if h.authorizer == nil {
			httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "user operations service is unavailable")
			return
		}
		resource := rbac.Resource{WorkspaceID: batch.WorkspaceID, Type: "workspace"}
		if err := h.authorizer.Authorize(r.Context(), actor.IdentityID, "operation.read", resource, actor.BreakGlass); err != nil {
			writeAuthorizationError(w, r, err)
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, batch)
}

func (h *Handler) userOperationMetrics(w http.ResponseWriter, r *http.Request) {
	if h.operations == nil {
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "user operations service is unavailable")
		return
	}
	metrics, err := h.operations.Metrics(r.Context(), h.requestInfo(r).WorkspaceID)
	if err != nil {
		h.writeUserOperationsError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, metrics)
}

func (h *Handler) writeUserOperationsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, useroperations.ErrInvalidRequest):
		httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the quota, expiry, or batch request failed validation")
	case errors.Is(err, useroperations.ErrVersionConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/stale-revision", "Resource revision is stale", "the user policy changed after this request was prepared")
	case errors.Is(err, useroperations.ErrIdempotencyConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/idempotency-conflict", "Idempotency conflict", "the Idempotency-Key was already used with different input")
	case errors.Is(err, useroperations.ErrNotFound), errors.Is(err, database.ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested user policy or batch does not exist")
	case errors.Is(err, rbac.ErrForbidden):
		httpx.WriteProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Access denied", "the principal is not authorized for this operation")
	case errors.Is(err, approvals.ErrNotReady):
		httpx.WriteProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/approval-required", "Independent approval is required", "bulk user disable requires a valid approval bound to the batch identifier")
	default:
		h.logger.ErrorContext(r.Context(), "user operations request failed", "error", err)
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "user operations state is temporarily unavailable")
	}
}

func writeAuthorizationError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, database.ErrNotFound) {
		httpx.WriteProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	httpx.WriteProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Access denied", "the principal is not authorized for this resource and action")
}
