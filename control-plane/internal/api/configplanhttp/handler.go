package configplanhttp

import (
	"errors"
	"net/http"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/api/httpx"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
)

// Handler owns ConfigPlan HTTP behavior, not authorization or service lifecycle.
type Handler struct {
	plans       Plans
	requestInfo func(*http.Request) RequestInfo
	secretUse   SecretUse
}

// New accepts authenticated request values and the parent's Secret-use check.
func New(requestInfo func(*http.Request) RequestInfo, secretUse SecretUse) *Handler {
	if requestInfo == nil {
		panic("configplanhttp: authenticated request accessor is required")
	}
	return &Handler{requestInfo: requestInfo, secretUse: secretUse}
}

// SetPlans injects business methods at startup, before HTTP starts.
func (h *Handler) SetPlans(plans Plans) { h.plans = plans }

func (h *Handler) requirePlans(w http.ResponseWriter, r *http.Request) bool {
	if h.plans == nil {
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service unavailable", "configuration plan service is unavailable")
		return false
	}
	return true
}

type createConfigPlanRequest struct {
	ExpectedRevision int64               `json:"expected_revision"`
	Template         configplan.Template `json:"template"`
	NodeVariables    map[string]string   `json:"node_variables,omitempty"`
	TTLSeconds       int64               `json:"ttl_seconds"`
	Reason           string              `json:"reason"`
}

type applyConfigPlanRequest struct {
	ApprovalID string `json:"approval_id"`
	Reason     string `json:"reason"`
}

func (h *Handler) createConfigPlan(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlans(w, r) {
		return
	}
	nodeID, err := httpx.ParseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "node_id must be UUIDv7")
		return
	}
	idempotencyKey, ok := httpx.RequireIdempotencyKey(w, r)
	if !ok {
		return
	}
	var body createConfigPlanRequest
	if !httpx.DecodeStrictJSON(w, r, &body) {
		return
	}
	for _, directive := range body.Template.Directives {
		if directive.SecretRef == nil {
			continue
		}
		if h.secretUse == nil {
			httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service unavailable", "secret reference service is unavailable")
			return
		}
		if !h.secretUse(w, r, directive.SecretRef.ID) {
			return
		}
	}
	info := h.requestInfo(r)
	value, replayed, err := h.plans.Create(r.Context(), configplan.CreateRequest{
		NodeID: nodeID, ExpectedRevision: body.ExpectedRevision, Template: body.Template,
		NodeVariables: body.NodeVariables, TTL: time.Duration(body.TTLSeconds) * time.Second,
		IdempotencyKey: idempotencyKey, ActorID: info.ActorID, ActorIdentityID: info.ActorIdentityID,
		ActorSessionID: info.ActorSessionID, RequestID: info.RequestID, Traceparent: info.Traceparent, Reason: body.Reason,
	})
	if err != nil {
		writeConfigPlanError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/api/v1/config-plans/"+value.ID.String())
	httpx.WriteJSON(w, http.StatusAccepted, value)
}

func (h *Handler) getConfigPlan(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("plan_id"))
	if err != nil || id.Version() != 7 {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "plan_id must be UUIDv7")
		return
	}
	if !h.requirePlans(w, r) {
		return
	}
	value, err := h.plans.Get(r.Context(), id)
	if err != nil {
		writeConfigPlanError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, value)
}

func (h *Handler) applyConfigPlan(w http.ResponseWriter, r *http.Request) {
	planID, err := httpx.ParseUUIDv7(r.PathValue("plan_id"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "plan_id must be UUIDv7")
		return
	}
	key, ok := httpx.RequireIdempotencyKey(w, r)
	if !ok {
		return
	}
	var body applyConfigPlanRequest
	if !httpx.DecodeStrictJSON(w, r, &body) {
		return
	}
	approvalID, err := httpx.ParseUUIDv7(body.ApprovalID)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "approval_id must be UUIDv7")
		return
	}
	if !h.requirePlans(w, r) {
		return
	}
	info := h.requestInfo(r)
	value, replayed, err := h.plans.Apply(r.Context(), configplan.ApplyRequest{PlanID: planID, ApprovalID: approvalID, IdempotencyKey: key, ActorID: info.ActorID, ActorIdentityID: info.ActorIdentityID, ActorSessionID: info.ActorSessionID, RequestID: info.RequestID, Traceparent: info.Traceparent, Reason: body.Reason})
	if err != nil {
		writeConfigPlanError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/api/v1/operations/"+value.ID)
	httpx.WriteJSON(w, http.StatusAccepted, value)
}

func writeConfigPlanError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, configplan.ErrInvalid), errors.Is(err, operationstore.ErrInvalidRequest):
		httpx.WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/config-plan-invalid", "Configuration plan is invalid", "the template, variables, revision, or lifetime is invalid")
	case errors.Is(err, configplan.ErrStaleRevision), errors.Is(err, operationstore.ErrStaleRevision):
		httpx.WriteProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/stale-revision", "Configuration revision is stale", "refresh configuration state and plan again")
	case errors.Is(err, approvals.ErrNotReady):
		httpx.WriteProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/approval-not-ready", "Approval is not ready", "an unexpired independent approval for this exact plan is required")
	case errors.Is(err, configplan.ErrCapability), errors.Is(err, operationstore.ErrCapabilityMissing):
		httpx.WriteProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/capability-unavailable", "Capability is unavailable", "the node cannot validate this configuration")
	case errors.Is(err, operationstore.ErrIdempotencyConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/idempotency-conflict", "Idempotency conflict", "the idempotency key was used for different configuration content")
	case errors.Is(err, operationstore.ErrConfigApplyActive):
		httpx.WriteProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/config-apply-active", "Configuration apply already active", "wait for the active apply or its reconciliation to reach a terminal state")
	case errors.Is(err, database.ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the configuration plan or node does not exist")
	default:
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/config-plan-unavailable", "Configuration planning unavailable", "configuration planning is temporarily unavailable")
	}
}
