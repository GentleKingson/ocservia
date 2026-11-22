package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	platformconfig "github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"github.com/google/uuid"
)

type principalKey struct{}
type workspaceKey struct{}

func (s *Server) authenticate(r *http.Request) (auth.Principal, error) {
	if s.hasOperationPrincipal(r) {
		return auth.Principal{Subject: "developer", Issuer: "development", BreakGlass: true}, nil
	}
	if s.auth == nil {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	return s.auth.Authenticate(r.Context(), cookie)
}

func (s *Server) authorizeRoute(r *http.Request, principal auth.Principal) (context.Context, error) {
	if r.URL.Path == "/api/v1/auth/change-password" {
		if s.auth == nil || principal.Issuer != auth.LocalIssuer || principal.BreakGlass || principal.IdentityID == uuid.Nil || principal.SessionID == uuid.Nil {
			return nil, auth.ErrForbidden
		}
		return context.WithValue(r.Context(), principalKey{}, principal), nil
	}
	if r.URL.Path == "/api/v1/auth/logout" || r.URL.Path == "/api/v1/workspaces" {
		return context.WithValue(r.Context(), principalKey{}, principal), nil
	}
	if principal.Issuer == "development" {
		ctx := context.WithValue(r.Context(), principalKey{}, principal)
		if r.URL.Path != "/api/v1/events/stream" {
			return ctx, nil
		}
		workspaceID, parseErr := uuid.Parse(strings.TrimSpace(r.URL.Query().Get("workspace_id")))
		if parseErr != nil || workspaceID.Version() != 7 || s.auth == nil {
			return nil, auth.ErrForbidden
		}
		if err := s.auth.WorkspaceExists(r.Context(), workspaceID); err != nil {
			return nil, err
		}
		return context.WithValue(ctx, workspaceKey{}, workspaceID), nil
	}
	if s.auth == nil {
		return nil, auth.ErrForbidden
	}

	var workspaceID uuid.UUID
	var err error
	resourceScoped := true
	switch {
	case r.PathValue("node_id") != "":
		nodeID, parseErr := parseUUIDv7(r.PathValue("node_id"))
		if parseErr != nil {
			return nil, database.ErrNotFound
		}
		workspaceID, err = s.auth.NodeWorkspace(r.Context(), nodeID)
	case r.PathValue("operation_id") != "":
		operationID, parseErr := parseUUIDv7(r.PathValue("operation_id"))
		if parseErr != nil {
			return nil, database.ErrNotFound
		}
		workspaceID, err = s.auth.OperationWorkspace(r.Context(), operationID)
	case r.PathValue("rollout_id") != "":
		rolloutID, parseErr := parseUUIDv7(r.PathValue("rollout_id"))
		if parseErr != nil || s.operations == nil {
			return nil, database.ErrNotFound
		}
		workspaceID, err = s.operations.RolloutWorkspace(r.Context(), rolloutID)
	case r.PathValue("plan_id") != "":
		planID, parseErr := parseUUIDv7(r.PathValue("plan_id"))
		if parseErr != nil || s.configPlans == nil {
			return nil, database.ErrNotFound
		}
		workspaceID, _, err = s.configPlans.Resource(r.Context(), planID)
	case r.PathValue("certificate_id") != "":
		certText := r.PathValue("certificate_id")
		certID, parseErr := parseUUIDv7(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(certText, ":revoke"), ":issue"), ":p12"))
		if parseErr != nil || s.certificates == nil {
			return nil, database.ErrNotFound
		}
		workspaceID, _, err = s.certificates.Resource(r.Context(), certID)
	case r.PathValue("artifact_id") != "":
		artifactID, parseErr := parseUUIDv7(r.PathValue("artifact_id"))
		if parseErr != nil || s.certificates == nil {
			return nil, database.ErrNotFound
		}
		workspaceID, _, err = s.certificates.ArtifactResource(r.Context(), artifactID)
	case r.PathValue("secret_ref_id") != "":
		secretText := r.PathValue("secret_ref_id")
		secretID, parseErr := parseUUIDv7(strings.TrimSuffix(secretText, ":rotate"))
		if parseErr != nil || s.certificates == nil {
			return nil, database.ErrNotFound
		}
		workspaceID, err = s.certificates.SecretRefResource(r.Context(), secretID)
	default:
		resourceScoped = false
		workspaceID, err = s.selectWorkspace(r, principal)
	}
	if err != nil {
		return nil, err
	}
	if resourceScoped {
		if err := s.auth.AuthorizeWorkspace(r.Context(), principal, workspaceID); err != nil {
			// Report a resource in an unauthorized workspace as 404 so a 403
			// never confirms that an invisible resource exists.
			if errors.Is(err, auth.ErrForbidden) {
				return nil, database.ErrNotFound
			}
			return nil, err
		}
	}
	ctx := context.WithValue(r.Context(), principalKey{}, principal)
	ctx = context.WithValue(ctx, workspaceKey{}, workspaceID)
	return ctx, nil
}

func (s *Server) selectWorkspace(r *http.Request, principal auth.Principal) (uuid.UUID, error) {
	workspaceText := strings.TrimSpace(r.Header.Get("X-Workspace-ID"))
	if workspaceText == "" && r.URL.Path == "/api/v1/events/stream" {
		workspaceText = strings.TrimSpace(r.URL.Query().Get("workspace_id"))
	}
	workspaces, err := s.auth.AuthorizedWorkspaces(r.Context(), principal)
	if err != nil {
		return uuid.Nil, err
	}
	if workspaceText == "" {
		if len(workspaces) != 1 {
			return uuid.Nil, auth.ErrForbidden
		}
		return workspaces[0].ID, nil
	}
	workspaceID, parseErr := parseUUIDv7(workspaceText)
	if parseErr != nil || !slices.ContainsFunc(workspaces, func(w authstore.Workspace) bool { return w.ID == workspaceID }) {
		return uuid.Nil, auth.ErrForbidden
	}
	return workspaceID, nil
}

func principal(r *http.Request) auth.Principal {
	value, _ := r.Context().Value(principalKey{}).(auth.Principal)
	return value
}

func workspace(r *http.Request) uuid.UUID {
	value, _ := r.Context().Value(workspaceKey{}).(uuid.UUID)
	return value
}

func (s *Server) writeAuthorizationError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, database.ErrNotFound) {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	if errors.Is(err, auth.ErrForbidden) {
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Access denied", "the principal is not authorized for this resource and action")
		return
	}
	// A failed workspace or resource lookup is not a denial.
	writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Authorization unavailable", "authorization state is temporarily unavailable")
}

func (s *Server) requireOperationAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, err := s.authenticate(r)
		if err != nil {
			writeProblem(w, r, http.StatusUnauthorized, "https://ocservia.dev/problems/unauthenticated", "Authentication required", "operation state requires an authenticated principal")
			return
		}
		if err := s.validateBrowserMutation(r, principal); err != nil {
			writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/cross-origin-request", "Cross-origin request", err.Error())
			return
		}
		ctx, err := s.authorizeRoute(r, principal)
		if err != nil {
			s.writeAuthorizationError(w, r, err)
			return
		}
		next(w, r.WithContext(ctx))
	}
}

var errCrossOrigin = errors.New("the request Origin does not match the trusted browser origin")

// validateBrowserMutation enforces the browser trust boundary for state
// changing requests: a session cookie established through Local or break-glass
// may only be spent by the exact public browser origin, so a sibling origin
// on the same site, an unknown site, or a missing Origin cannot drive a
// mutation. Development bearer principals are non-browser credentials and
// safe methods never mutate state, so neither requires an Origin. A
// cross-site Fetch Metadata signal is rejected even before the Origin
// comparison, while same-site and same-origin signals never replace it.
func (s *Server) validateBrowserMutation(r *http.Request, principal auth.Principal) error {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil
	}
	if principal.Issuer == "development" {
		return nil
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return errCrossOrigin
	}
	if s.browserOrigin == "" {
		return errCrossOrigin
	}
	origin, ok := platformconfig.NormalizeBrowserOrigin(r.Header.Get("Origin"))
	if !ok || origin != s.browserOrigin {
		return errCrossOrigin
	}
	return nil
}

// hasOperationPrincipal admits the development principal only through a
// development Host, so a DNS-rebound browser page cannot spend the public
// development token or the unauthenticated DevAuth mode on any API route.
func (s *Server) hasOperationPrincipal(r *http.Request) bool {
	return s.hasOperationCredential(r) && developmentHost(r.Host)
}

func (s *Server) hasOperationCredential(r *http.Request) bool {
	if s.devAuth {
		return true
	}
	const prefix = "Bearer "
	authorization := r.Header.Get("Authorization")
	if s.devAuthToken == "" || !strings.HasPrefix(authorization, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(strings.TrimPrefix(authorization, prefix))), []byte(s.devAuthToken)) == 1
}
