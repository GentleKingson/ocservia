package api

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/google/uuid"
)

type bootstrapTokenRequest struct {
	Installation       *bool  `json:"installation"`
	ExpectedEndpointID string `json:"expected_endpoint_id"`
	WorkspaceID        string `json:"workspace_id"`
	Environment        string `json:"environment"`
	ExpectedNodeName   string `json:"expected_node_name"`
	TTLSeconds         int64  `json:"ttl_seconds"`
	Reason             string `json:"reason"`
}

type revocationRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) createNodeBootstrapToken(w http.ResponseWriter, r *http.Request) {
	if s.enrollment == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "enrollment is not enabled")
		return
	}
	var body bootstrapTokenRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	if body.Installation != nil && !*body.Installation {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "installation tokens are required; obt1_ bootstrap tokens are retired")
		return
	}
	workspaceID, err := parseUUIDv7(body.WorkspaceID)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "workspace_id must be UUIDv7")
		return
	}
	if principal(r).Issuer != "development" && workspaceID != workspace(r) {
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Access denied", "workspace_id is outside the authorized scope")
		return
	}
	var endpoint []byte
	if body.ExpectedEndpointID != "" {
		var err error
		endpoint, err = hex.DecodeString(body.ExpectedEndpointID)
		if err != nil || len(endpoint) != 32 || strings.ToLower(body.ExpectedEndpointID) != body.ExpectedEndpointID {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "expected_endpoint_id must be 32-byte lowercase hex")
			return
		}
	}
	if !validEnrollmentTTLSeconds(body.TTLSeconds) {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "ttl_seconds must be between 1 and 900")
		return
	}
	token, err := s.enrollment.CreateInstallationToken(r.Context(), enrollment.BootstrapTokenSpec{
		ExpectedEndpointID: endpoint, WorkspaceID: workspaceID, Environment: body.Environment,
		ExpectedNodeName: body.ExpectedNodeName, TTL: time.Duration(body.TTLSeconds) * time.Second,
		ActorID: actorID(r), Reason: body.Reason, RequestID: requestID(r),
	})
	if err != nil {
		s.writeEnrollmentError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"id": token.ID, "token": token.Value, "expires_at": token.ExpiresAt})
}

func validEnrollmentTTLSeconds(seconds int64) bool {
	return seconds == 0 || seconds >= 1 && seconds <= int64(enrollment.DefaultTokenTTL/time.Second)
}

func (s *Server) revokeNode(w http.ResponseWriter, r *http.Request) {
	if s.enrollment == nil || s.transport == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "enrollment is not enabled")
		return
	}
	nodeID, err := parseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "node_id must be UUIDv7")
		return
	}
	var body revocationRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	actor := principal(r)
	trust, err := s.enrollment.Revoke(r.Context(), enrollment.Revocation{
		NodeID: nodeID, ActorID: actorID(r), IdentityID: actor.IdentityID, SessionID: actor.SessionID,
		Reason: body.Reason, RequestID: requestID(r),
	})
	if err != nil {
		s.writeEnrollmentError(w, r, err)
		return
	}
	operationID := ownersession.StateUpdateOperationID([16]byte(nodeID), trust.EndpointID, int32(transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED), trust.Revision, body.Reason)
	syncErr := s.adminExecuteFenced(r.Context(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_STATE_UPDATE, operationID,
		func(ctx context.Context, _ *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
			return s.transport.UpdateNodeTrust(ctx, trust.NodeID[:], trust.EndpointID, transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED, body.Reason, trust.Revision, operationID[:], binding)
		})
	var closeErr error
	if syncErr == nil {
		// A fenced close must carry its binding: swallowing the failure would
		// send a binding-less close that transportd rejects anyway.
		closeErr = s.adminExecuteFenced(r.Context(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_CONNECTION_CLOSE, [16]byte(nodeID),
			func(ctx context.Context, _ *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
				return s.transport.CloseNode(ctx, trust.NodeID[:], "node revoked", binding)
			})
	}
	if syncErr == nil {
		// Only this request's fenced successes settle the queued intent; a
		// failed close stays with the worker.
		if _, err := s.enrollment.RecordRevocationSynchronized(r.Context(), trust, body.Reason, closeErr == nil); err != nil {
			s.logger.WarnContext(r.Context(), "record synchronous revocation convergence", "error", err)
		}
	}
	if syncErr != nil || closeErr != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/transport-unavailable", "Revocation committed", "the node is revoked but transport disconnect synchronization is pending")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": nodeID, "status": "revoked"})
}

// adminExecuteFenced runs one administrative transport mutation inside the
// connection owner's fencing interval. The binding capability is the fencing
// capability itself; nodes without a registered fence keep the unfenced path.
func (s *Server) adminExecuteFenced(ctx context.Context, nodeID uuid.UUID, kind agentv1.FenceOperationKind, operationID [16]byte, action ownersession.FencedAction) error {
	if s.fences == nil {
		return action(ctx, nil, nil)
	}
	var fixed [16]byte
	copy(fixed[:], nodeID[:])
	return s.fences.ExecuteFenced(ctx, fixed, kind, operationID, ownersession.FencingCapability, action)
}

func (s *Server) writeEnrollmentError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrForbidden):
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Forbidden", "the current session is not authorized for this operation")
	case errors.Is(err, enrollment.ErrInvalidTransition):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/invalid-node-state", "Invalid node state", "the requested node transition is not allowed")
	case errors.Is(err, enrollment.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
	case errors.Is(err, enrollment.ErrInvalidRequest):
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "request could not be completed")
	default:
		s.logger.ErrorContext(r.Context(), "enrollment persistence failed", "error", err)
		w.Header().Set("Retry-After", "1")
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Enrollment service unavailable", "the enrollment database is temporarily unavailable")
	}
}
