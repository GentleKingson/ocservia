package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type breakGlassRequest struct {
	Token string `json:"token"`
}

type localLoginRequest struct {
	Username *string `json:"username"`
	Password *string `json:"password"`
}

func (s *Server) localLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.auth == nil || !s.auth.LocalEnabled() {
		s.logAuth(r, localDisabled, uuid.Nil, "")
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "Local authentication is not configured")
		return
	}
	if err := s.validateBrowserMutation(r, auth.Principal{Issuer: auth.LocalIssuer}); err != nil {
		s.logAuth(r, localOriginRejected, uuid.Nil, "")
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/cross-origin-request", "Cross-origin request", err.Error())
		return
	}
	release := s.admitAuthentication(w, r, s.localLoginBudget)
	if release == nil {
		return
	}
	defer release()
	// Allow JSON escaping of the core's 128-byte username and 1024-byte password.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var body localLoginRequest
	if !decodeStrictJSON(w, r, &body) {
		s.logAuth(r, localInvalidRequest, uuid.Nil, "")
		return
	}
	if body.Username == nil || body.Password == nil {
		s.logAuth(r, localInvalidRequest, uuid.Nil, "")
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "username and password are required")
		return
	}
	// AuthenticateLocal enforces field byte limits and hides credential existence.
	account := s.auth.LocalAccountRef(*body.Username)
	cookie, actor, err := s.auth.AuthenticateLocal(r.Context(), *body.Username, *body.Password)
	if err != nil {
		kind := localUnavailable
		switch {
		case errors.Is(err, auth.ErrLocalAccountLimited):
			kind = localAccountLimited
		case errors.Is(err, auth.ErrLocalAttemptCapacity):
			kind = localCapacityLimited
		case errors.Is(err, auth.ErrUnauthenticated):
			kind = localCredentialsRejected
		}
		s.logAuth(r, kind, uuid.Nil, account)
		// Account cooldown/single-flight rejection deliberately shares the bad
		// credential response, without an account-specific Retry-After header.
		if errors.Is(err, auth.ErrUnauthenticated) {
			writeProblem(w, r, http.StatusUnauthorized, "https://ocservia.dev/problems/unauthenticated", "Invalid username or password", "Invalid username or password")
		} else {
			writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/authentication-unavailable", "Login unavailable", "Local login could not be completed")
		}
		return
	}
	http.SetCookie(w, cookie)
	s.logAuth(r, localSucceeded, actor.IdentityID, account)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	value := principal(r)
	if s.auth != nil && value.SessionID != [16]byte{} {
		if err := s.auth.Logout(r.Context(), value); err != nil {
			writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Logout unavailable", "the session could not be revoked")
			return
		}
	}
	http.SetCookie(w, auth.ClearCookie(auth.SessionCookieName))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) changeLocalPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	err := s.auth.ChangeLocalPassword(r.Context(), principal(r), body.CurrentPassword, body.NewPassword, requestID(r))
	if err != nil {
		status, detail := http.StatusInternalServerError, "password change failed"
		if errors.Is(err, auth.ErrPasswordPolicy) {
			status, detail = http.StatusBadRequest, err.Error()
		}
		if errors.Is(err, auth.ErrUnauthenticated) {
			status, detail = http.StatusUnauthorized, "current password or Local session is invalid, or attempts are temporarily blocked"
		}
		if errors.Is(err, auth.ErrLocalDisabled) {
			status, detail = http.StatusForbidden, "Local authentication is disabled"
		}
		writeProblem(w, r, status, "https://ocservia.dev/problems/password-change", "Password change failed", detail)
		return
	}
	http.SetCookie(w, auth.ClearCookie(auth.SessionCookieName))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) breakGlass(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.auth == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "break-glass is not configured")
		return
	}
	if err := s.validateBrowserMutation(r, auth.Principal{Issuer: "break-glass"}); err != nil {
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/cross-origin-request", "Cross-origin request", err.Error())
		return
	}
	release := s.admitAuthentication(w, r, s.breakGlassBudget)
	if release == nil {
		return
	}
	defer release()
	var body breakGlassRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Token) == "" {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "token is required")
		return
	}
	cookie, _, err := s.auth.BreakGlass(r.Context(), body.Token, requestID(r))
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, auth.ErrBreakGlassDisabled) {
			status = http.StatusNotFound
		}
		if errors.Is(err, auth.ErrBreakGlassRotationDue) {
			status = http.StatusLocked
		}
		writeProblem(w, r, status, "https://ocservia.dev/problems/break-glass-rejected", "Break-glass rejected", "emergency access is disabled, invalid, or requires credential rotation")
		return
	}
	http.SetCookie(w, cookie)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	actor := principal(r)
	var items []authstore.Workspace
	if actor.Issuer == "development" {
		if s.backend == nil {
			writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Workspaces unavailable", "authorized workspaces could not be read")
			return
		}
		err := database.Within(r.Context(), s.backend, database.ReadCommitted, func(tx database.Tx) error {
			store, err := authstore.From(tx)
			if err != nil {
				return err
			}
			items, err = store.AuthorizedWorkspaces(r.Context(), uuid.Nil, true)
			return err
		})
		if err != nil {
			writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Workspaces unavailable", "authorized workspaces could not be read")
			return
		}
	} else {
		if s.auth == nil {
			writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Workspaces unavailable", "authorized workspaces could not be read")
			return
		}
		var err error
		items, err = s.auth.AuthorizedWorkspaces(r.Context(), actor)
		if err != nil {
			writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Workspaces unavailable", "authorized workspaces could not be read")
			return
		}
	}
	if items == nil {
		items = []authstore.Workspace{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
