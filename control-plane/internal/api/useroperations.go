package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/google/uuid"
)

type UserOperations interface {
	GetPolicy(context.Context, uuid.UUID, string) (useroperations.Policy, error)
	SetPolicy(context.Context, useroperations.PolicyRequest) (useroperations.Policy, bool, error)
}

var _ UserOperations = (*useroperations.Service)(nil)

type userPolicyRequest struct {
	QuotaPeriod     string  `json:"quota_period"`
	QuotaDirection  string  `json:"quota_direction"`
	QuotaBytes      int64   `json:"quota_bytes"`
	ExpiresAt       *string `json:"expires_at"`
	ExpectedVersion int64   `json:"expected_version"`
	Reason          string  `json:"reason"`
}

func (s *Server) getUserPolicy(w http.ResponseWriter, r *http.Request) {
	if s.userOperations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "user operations service is unavailable")
		return
	}
	nodeID, err := parseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "node_id must be a UUIDv7")
		return
	}
	policy, err := s.userOperations.GetPolicy(r.Context(), nodeID, r.PathValue("username"))
	if err != nil {
		s.writeUserOperationsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) setUserPolicy(w http.ResponseWriter, r *http.Request) {
	if s.userOperations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "user operations service is unavailable")
		return
	}
	nodeID, err := parseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "node_id must be a UUIDv7")
		return
	}
	idempotencyKey, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	var body userPolicyRequest
	if !decodeStrictJSON(w, r, &body) {
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
	policy, replayed, err := s.userOperations.SetPolicy(r.Context(), useroperations.PolicyRequest{
		NodeID: nodeID, Username: r.PathValue("username"),
		QuotaPeriod: body.QuotaPeriod, QuotaDirection: body.QuotaDirection, QuotaBytes: body.QuotaBytes,
		ExpiresAt: expiresAt, ExpectedVersion: body.ExpectedVersion, IdempotencyKey: idempotencyKey,
		ActorID: actorID(r), ActorIdentityID: actor.IdentityID, ActorSessionID: actor.SessionID,
		Reason: strings.TrimSpace(body.Reason), RequestID: requestID(r), Traceparent: requestTraceparent(r),
	})
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

func (s *Server) writeUserOperationsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, useroperations.ErrInvalidRequest):
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the quota or expiry request failed validation")
	case errors.Is(err, useroperations.ErrVersionConflict):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/stale-revision", "Resource revision is stale", "the user policy changed after this request was prepared")
	case errors.Is(err, useroperations.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/idempotency-conflict", "Idempotency conflict", "the Idempotency-Key was already used with different input")
	case errors.Is(err, useroperations.ErrNotFound), errors.Is(err, database.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested user policy does not exist")
	case errors.Is(err, auth.ErrForbidden):
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Access denied", "the principal is not authorized for this operation")
	default:
		s.logger.ErrorContext(r.Context(), "user operations request failed", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "user operations state is temporarily unavailable")
	}
}
