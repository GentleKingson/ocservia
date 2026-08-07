package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type createConfigPlanRequest struct {
	ExpectedRevision int64               `json:"expected_revision"`
	Template         configplan.Template `json:"template"`
	NodeVariables    map[string]string   `json:"node_variables,omitempty"`
	TTLSeconds       int64               `json:"ttl_seconds"`
	Reason           string              `json:"reason"`
}

func (s *Server) createConfigPlan(w http.ResponseWriter, r *http.Request) {
	if s.configplans == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service unavailable", "configuration plan service is unavailable")
		return
	}
	nodeID, err := parseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "node_id must be UUIDv7")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/idempotency-key-required", "Idempotency key is required", "Idempotency-Key must be provided")
		return
	}
	var body createConfigPlanRequest
	if !decodeStrict(w, r, &body) {
		return
	}
	actor := principal(r)
	value, replayed, err := s.configplans.Create(r.Context(), configplan.CreateRequest{
		NodeID: nodeID, ExpectedRevision: body.ExpectedRevision, Template: body.Template,
		NodeVariables: body.NodeVariables, TTL: time.Duration(body.TTLSeconds) * time.Second,
		IdempotencyKey: idempotencyKey, ActorID: actorID(r), ActorIdentityID: actor.IdentityID,
		ActorSessionID: actor.SessionID, RequestID: requestID(r), Traceparent: requestTraceparent(r), Reason: body.Reason,
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
	id, err := uuid.Parse(r.PathValue("plan_id"))
	if err != nil || id.Version() != 7 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "plan_id must be UUIDv7")
		return
	}
	value, err := s.configplans.Get(r.Context(), id)
	if err != nil {
		writeConfigPlanError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func writeConfigPlanError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, configplan.ErrInvalid), errors.Is(err, operationstore.ErrInvalidRequest):
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/config-plan-invalid", "Configuration plan is invalid", "the template, variables, revision, or lifetime is invalid")
	case errors.Is(err, configplan.ErrStaleRevision), errors.Is(err, operationstore.ErrStaleRevision):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/stale-revision", "Configuration revision is stale", "refresh configuration state and plan again")
	case errors.Is(err, configplan.ErrCapability), errors.Is(err, operationstore.ErrCapabilityMissing):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/capability-unavailable", "Capability is unavailable", "the node cannot validate this configuration")
	case errors.Is(err, operationstore.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/idempotency-conflict", "Idempotency conflict", "the idempotency key was used for different configuration content")
	case errors.Is(err, pgx.ErrNoRows):
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the configuration plan or node does not exist")
	default:
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/config-plan-unavailable", "Configuration planning unavailable", "configuration planning is temporarily unavailable")
	}
}
