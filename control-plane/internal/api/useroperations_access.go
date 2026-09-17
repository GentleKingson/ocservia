package api

import (
	"net/http"

	"github.com/GentleKingson/ocservia/control-plane/internal/api/useroperationshttp"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
)

var _ useroperationshttp.Operations = (*useroperations.Service)(nil)
var _ useroperationshttp.Authorizer = (*rbac.Service)(nil)

// EnableUserOperations injects the existing configured service at startup.
// Explicitly convert typed nil so clearing the module fails closed.
func (s *Server) EnableUserOperations(service *useroperations.Service) {
	if service == nil {
		s.userOpsHTTP.SetOperations(nil)
		return
	}
	s.userOpsHTTP.SetOperations(service)
}

func userOperationsRequestInfo(r *http.Request) useroperationshttp.RequestInfo {
	return useroperationshttp.RequestInfo{
		Principal: principal(r), WorkspaceID: workspace(r), ActorID: actorID(r),
		RequestID: requestID(r), Traceparent: requestTraceparent(r), ApprovalID: approvalID(r),
	}
}
