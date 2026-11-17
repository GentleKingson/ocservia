package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
)

type ConfigPlans interface {
	Create(context.Context, configplan.CreateRequest) (configplan.Plan, bool, error)
	Get(context.Context, uuid.UUID) (configplan.Plan, error)
	Apply(context.Context, configplan.ApplyRequest) (operations.Operation, bool, error)
	Resource(context.Context, uuid.UUID) (workspaceID, nodeID uuid.UUID, err error)
}

var _ ConfigPlans = (*configplan.Service)(nil)

type createConfigPlanRequest struct {
	ExpectedRevision int64               `json:"expected_revision"`
	Template         configplan.Template `json:"template"`
	TTLSeconds       int64               `json:"ttl_seconds"`
	Reason           string              `json:"reason"`
}

type applyConfigPlanRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) createConfigPlan(w http.ResponseWriter, r *http.Request) {
	if s.configPlans == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service unavailable", "configuration plan service is unavailable")
		return
	}
	nodeID, err := parseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "node_id must be UUIDv7")
		return
	}
	idempotencyKey, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	var body createConfigPlanRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	for _, directive := range body.Template.Directives {
		if directive.SecretRef == nil {
			continue
		}
		if !s.allowConfigPlanSecret(w, r, directive.SecretRef.ID) {
			return
		}
	}
	actor := principal(r)
	value, replayed, err := s.configPlans.Create(r.Context(), configplan.CreateRequest{
		NodeID: nodeID, ExpectedRevision: body.ExpectedRevision, Template: body.Template,
		TTL: time.Duration(body.TTLSeconds) * time.Second, IdempotencyKey: idempotencyKey,
		ActorID: actorID(r), ActorIdentityID: actor.IdentityID, ActorSessionID: actor.SessionID,
		RequestID: requestID(r), Traceparent: requestTraceparent(r), Reason: body.Reason,
	})
	if err != nil {
		writeConfigPlanError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/api/v1/config-plans/"+value.ID.String())
	writeJSON(w, http.StatusAccepted, value)
}

func (s *Server) getConfigPlan(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDv7(r.PathValue("plan_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "plan_id must be UUIDv7")
		return
	}
	if s.configPlans == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service unavailable", "configuration plan service is unavailable")
		return
	}
	value, err := s.configPlans.Get(r.Context(), id)
	if err != nil {
		writeConfigPlanError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) applyConfigPlan(w http.ResponseWriter, r *http.Request) {
	planID, err := parseUUIDv7(r.PathValue("plan_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "plan_id must be UUIDv7")
		return
	}
	key, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	var body applyConfigPlanRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	if s.configPlans == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service unavailable", "configuration plan service is unavailable")
		return
	}
	actor := principal(r)
	value, replayed, err := s.configPlans.Apply(r.Context(), configplan.ApplyRequest{
		PlanID: planID, IdempotencyKey: key,
		ActorID: actorID(r), ActorIdentityID: actor.IdentityID, ActorSessionID: actor.SessionID,
		RequestID: requestID(r), Traceparent: requestTraceparent(r), Reason: body.Reason,
	})
	if err != nil {
		writeConfigPlanError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/api/v1/operations/"+value.ID)
	writeJSON(w, http.StatusAccepted, value)
}

func (s *Server) allowConfigPlanSecret(w http.ResponseWriter, r *http.Request, id uuid.UUID) bool {
	if s.certificates == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service unavailable", "secret reference service is unavailable")
		return false
	}
	workspaceID, err := s.certificates.SecretRefResource(r.Context(), id)
	if err != nil {
		s.writeAuthorizationError(w, r, auth.ErrForbidden)
		return false
	}
	actor := principal(r)
	if actor.Issuer == "development" {
		if target := workspace(r); target != uuid.Nil && workspaceID != target {
			s.writeAuthorizationError(w, r, auth.ErrForbidden)
			return false
		}
		return true
	}
	if workspaceID != workspace(r) || s.auth == nil || s.auth.AuthorizeWorkspace(r.Context(), actor, workspaceID) != nil {
		s.writeAuthorizationError(w, r, auth.ErrForbidden)
		return false
	}
	return true
}

func writeConfigPlanError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrForbidden):
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Access denied", "the session or resource authority is no longer valid")
	case errors.Is(err, configplan.ErrInvalid), errors.Is(err, operations.ErrInvalidRequest):
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/config-plan-invalid", "Configuration plan is invalid", "the template, variables, revision, or lifetime is invalid")
	case errors.Is(err, configplan.ErrStaleRevision), errors.Is(err, operations.ErrStaleRevision):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/stale-revision", "Configuration revision is stale", "refresh configuration state and plan again")
	case errors.Is(err, configplan.ErrCapability), errors.Is(err, operations.ErrCapabilityMissing):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/capability-unavailable", "Capability is unavailable", "the node cannot validate this configuration")
	case errors.Is(err, operations.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/idempotency-conflict", "Idempotency conflict", "the idempotency key was used for different configuration content")
	case errors.Is(err, operations.ErrConfigApplyActive):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/config-apply-active", "Configuration apply already active", "wait for the active apply or its reconciliation to reach a terminal state")
	case errors.Is(err, database.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the configuration plan or node does not exist")
	default:
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/config-plan-unavailable", "Configuration planning unavailable", "configuration planning is temporarily unavailable")
	}
}
