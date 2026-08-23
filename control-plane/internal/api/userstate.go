package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/google/uuid"
)

type desiredMutationRequest struct {
	Name            string               `json:"name,omitempty"`
	Members         []string             `json:"members,omitempty"`
	SealedPassword  *sealedSecretRequest `json:"sealed_password,omitempty"`
	ExpectedVersion *int64               `json:"expected_version,omitempty"`
	TTLSeconds      *int64               `json:"ttl_seconds,omitempty"`
	Reason          string               `json:"reason"`
}

type sealedSecretRequest struct {
	Version    uint32 `json:"version"`
	Purpose    string `json:"purpose"`
	KeyID      string `json:"key_id"`
	Ciphertext []byte `json:"ciphertext"`
}

func (s *Server) listUserGroupState(w http.ResponseWriter, r *http.Request) {
	if s.userstate == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "user state service is unavailable")
		return
	}
	nodeID, ok := pathUUIDv7(r.PathValue("node_id"))
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "node_id must be a UUIDv7")
		return
	}
	items, err := s.userstate.List(r.Context(), nodeID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "list user group state failed", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "desired and observed state is temporarily unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	s.mutateUserGroup(w, r, userstate.UserCreate, "")
}

func (s *Server) userAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("user_action")
	switch {
	case strings.HasSuffix(action, ":disable"):
		s.mutateUserGroup(w, r, userstate.UserDisable, strings.TrimSuffix(action, ":disable"))
	case strings.HasSuffix(action, ":enable"):
		s.mutateUserGroup(w, r, userstate.UserEnable, strings.TrimSuffix(action, ":enable"))
	case strings.HasSuffix(action, ":rotate-password"):
		s.mutateUserGroup(w, r, userstate.UserPasswordRotate, strings.TrimSuffix(action, ":rotate-password"))
	default:
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
	}
}

func (s *Server) applyGroup(w http.ResponseWriter, r *http.Request) {
	s.mutateUserGroup(w, r, userstate.GroupApply, r.PathValue("group_name"))
}

func (s *Server) mutateUserGroup(w http.ResponseWriter, r *http.Request, kind userstate.MutationKind, pathName string) {
	if s.userstate == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "user state service is unavailable")
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
	var body desiredMutationRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	name := pathName
	if kind == userstate.UserCreate {
		name = body.Name
	}
	expected, ok := desiredExpectedVersion(r.Header.Get("If-Match"), body.ExpectedVersion)
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/expected-version-required", "Expected version is invalid", "provide If-Match revision-N or expected_version; use revision-0 when creating")
		return
	}
	ttl := int64(86400)
	if body.TTLSeconds != nil {
		ttl = *body.TTLSeconds
	}
	actor := principal(r)
	actorText := actorID(r)
	var sealedPassword *userstate.SealedSecret
	if body.SealedPassword != nil {
		sealedPassword = &userstate.SealedSecret{Version: body.SealedPassword.Version, Purpose: strings.TrimSpace(body.SealedPassword.Purpose), KeyID: strings.TrimSpace(body.SealedPassword.KeyID), Ciphertext: body.SealedPassword.Ciphertext}
	}
	operation, replayed, err := s.userstate.Mutate(r.Context(), userstate.MutationRequest{
		NodeID: nodeID, Kind: kind, Name: name, Members: body.Members,
		SealedPassword: sealedPassword,
		IdempotencyKey: idempotencyKey, ExpectedVersion: expected, TTL: time.Duration(ttl) * time.Second,
		ActorID: actorText, ActorIdentityID: actor.IdentityID, ActorSessionID: actor.SessionID,
		Reason: strings.TrimSpace(body.Reason), RequestID: requestID(r), Traceparent: requestTraceparent(r),
	})
	if err != nil {
		s.writeUserStateError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
	w.Header().Set("ETag", fmt.Sprintf("\"revision-%d\"", operation.Version))
	writeJSON(w, http.StatusAccepted, operation)
}

func desiredExpectedVersion(ifMatch string, explicit *int64) (int64, bool) {
	var header *int64
	if ifMatch != "" {
		if len(ifMatch) < 3 || ifMatch[0] != '"' || ifMatch[len(ifMatch)-1] != '"' {
			return 0, false
		}
		value := ifMatch[1 : len(ifMatch)-1]
		if !strings.HasPrefix(value, "revision-") {
			return 0, false
		}
		parsed, err := strconv.ParseInt(strings.TrimPrefix(value, "revision-"), 10, 64)
		if err != nil || parsed < 0 {
			return 0, false
		}
		header = &parsed
	}
	if explicit != nil && *explicit < 0 {
		return 0, false
	}
	if explicit != nil && header != nil && *explicit != *header {
		return 0, false
	}
	if explicit != nil {
		return *explicit, true
	}
	if header != nil {
		return *header, true
	}
	return 0, false
}

func pathUUIDv7(value string) (uuid.UUID, bool) {
	id, err := uuid.Parse(value)
	return id, err == nil && id.Version() == 7
}

func (s *Server) writeUserStateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, userstate.ErrInvalidRequest):
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the desired state request failed validation")
	case errors.Is(err, userstate.ErrCapacityExceeded):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/capacity-exceeded", "Capacity is exceeded", "the node cannot accept another managed user, group, or membership")
	case errors.Is(err, userstate.ErrBacklogExceeded):
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/command-backlog-exceeded", "Command backlog is full", "the node or workspace remote command backlog has reached its bound")
	case errors.Is(err, userstate.ErrVersionConflict):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/stale-revision", "Resource revision is stale", "the desired resource changed after this request was prepared")
	case errors.Is(err, userstate.ErrRevisionPending):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/desired-revision-pending", "Desired revision is pending", "wait for the current desired revision to finish before changing this resource")
	case errors.Is(err, userstate.ErrRevisionRecovery):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/desired-revision-recovery-required", "Desired revision requires recovery", "replace the failed desired mutation with the same mutation kind before changing another property")
	case errors.Is(err, userstate.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/idempotency-conflict", "Idempotency conflict", "the Idempotency-Key was already used with different input")
	case errors.Is(err, userstate.ErrCapabilityMissing):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/capability-unavailable", "Capability is unavailable", "the node has not advertised and received approval for this operation")
	case errors.Is(err, userstate.ErrNodeUnavailable), errors.Is(err, userstate.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested node or desired resource does not exist")
	default:
		s.logger.ErrorContext(r.Context(), "user state request failed", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "desired state is temporarily unavailable")
	}
}
