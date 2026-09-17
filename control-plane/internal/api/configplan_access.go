package api

import (
	"context"
	"net/http"

	"github.com/GentleKingson/ocservia/control-plane/internal/api/configplanhttp"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/google/uuid"
)

// configPlanLookup is the read-only view used by authorization and approvals.
type configPlanLookup interface {
	Get(context.Context, uuid.UUID) (configplan.Plan, error)
	Resource(context.Context, uuid.UUID) (workspaceID, nodeID uuid.UUID, err error)
}

var _ configPlanLookup = (*configplan.Service)(nil)
var _ configplanhttp.Plans = (*configplan.Service)(nil)

// EnableConfigPlans supplies two capability views of the same instance before
// HTTP starts. Convert typed nil explicitly and clear both views together.
func (s *Server) EnableConfigPlans(service *configplan.Service) {
	if service == nil {
		s.configPlanHTTP.SetPlans(nil)
		s.configPlanLookup = nil
		return
	}
	s.configPlanHTTP.SetPlans(service)
	s.configPlanLookup = service
}

func configPlanRequestInfo(r *http.Request) configplanhttp.RequestInfo {
	actor := principal(r)
	return configplanhttp.RequestInfo{
		ActorID: actorID(r), ActorIdentityID: actor.IdentityID, ActorSessionID: actor.SessionID,
		RequestID: requestID(r), Traceparent: requestTraceparent(r),
	}
}

// Resolve startup configuration at request time: Certificates and RBAC may be
// injected after ConfigPlans. This adapter grants no other Server capabilities.
func (s *Server) allowConfigPlanSecret(w http.ResponseWriter, r *http.Request, id uuid.UUID) bool {
	if s.certificates == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service unavailable", "secret reference service is unavailable")
		return false
	}
	workspaceID, err := s.certificates.SecretRefResource(r.Context(), id)
	if err != nil || workspaceID != workspace(r) {
		s.writeAuthorizationError(w, r, rbac.ErrForbidden)
		return false
	}
	actor := principal(r)
	if !s.devAuth && (s.rbac == nil || s.rbac.Authorize(r.Context(), actor.IdentityID, "secret.use", rbac.Resource{WorkspaceID: workspaceID, Type: "secret_ref", ID: id}, actor.BreakGlass) != nil) {
		s.writeAuthorizationError(w, r, rbac.ErrForbidden)
		return false
	}
	return true
}
