package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
)

type breakGlassRequest struct {
	Token string `json:"token"`
}

type localLoginRequest struct {
	Username *string `json:"username"`
	Password *string `json:"password"`
}

func (s *Server) authMethods(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, struct {
		Local bool `json:"local"`
		OIDC  bool `json:"oidc"`
	}{s.auth != nil && s.auth.LocalEnabled(), s.auth != nil && s.auth.OIDCEnabled()})
}

func (s *Server) localLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.auth == nil || !s.auth.LocalEnabled() {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "Local authentication is not configured")
		return
	}
	if err := s.validateBrowserMutation(r, auth.Principal{Issuer: auth.LocalIssuer}); err != nil {
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/cross-origin-request", "Cross-origin request", err.Error())
		return
	}
	release := s.admitAuthentication(w, r, s.localLoginBudget, false)
	if release == nil {
		return
	}
	defer release()
	// Allow JSON escaping of the core's 128-byte username and 1024-byte password.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var body localLoginRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	if body.Username == nil || body.Password == nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "username and password are required")
		return
	}
	// AuthenticateLocal enforces field byte limits and hides credential existence.
	cookie, _, err := s.auth.AuthenticateLocal(r.Context(), *body.Username, *body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) {
			writeProblem(w, r, http.StatusUnauthorized, "https://ocservia.dev/problems/unauthenticated", "Invalid username or password", "Invalid username or password")
		} else {
			writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/authentication-unavailable", "Login unavailable", "Local login could not be completed")
		}
		return
	}
	http.SetCookie(w, cookie)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || !s.auth.OIDCEnabled() {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "OIDC authentication is not configured")
		return
	}
	location, cookie, err := s.auth.BeginLogin(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "begin OIDC login", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/identity-provider-unavailable", "Login unavailable", "OIDC login could not be started")
		return
	}
	http.SetCookie(w, cookie)
	http.Redirect(w, r, location, http.StatusFound)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || !s.auth.OIDCEnabled() {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "OIDC authentication is not configured")
		return
	}
	loginCookie, _ := r.Cookie(auth.LoginCookieName)
	http.SetCookie(w, auth.ClearCookie(auth.LoginCookieName))
	sessionCookie, _, err := s.auth.CompleteLogin(r.Context(), r.URL.Query().Get("state"), r.URL.Query().Get("code"), loginCookie)
	if err != nil {
		s.logger.WarnContext(r.Context(), "OIDC callback rejected", "error", err)
		writeProblem(w, r, http.StatusUnauthorized, "https://ocservia.dev/problems/oidc-callback-rejected", "Login rejected", "the OIDC response could not be validated")
		return
	}
	http.SetCookie(w, sessionCookie)
	http.Redirect(w, r, "/", http.StatusFound)
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

func (s *Server) breakGlass(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "break-glass is not configured")
		return
	}
	// Break-glass establishes a high-privilege session cookie, so it takes
	// the same exact-origin boundary as the mutations that cookie authorizes;
	// a cross-site page must not be able to drive an emergency login.
	if err := s.validateBrowserMutation(r, auth.Principal{Issuer: "break-glass"}); err != nil {
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/cross-origin-request", "Cross-origin request", err.Error())
		return
	}
	var body breakGlassRequest
	if !decodeStrictJSON(w, r, &body) || strings.TrimSpace(body.Token) == "" {
		return
	}
	// Failed requests cannot lock out the holder of the emergency credential.
	// Valid credentials still share a separate, bounded in-flight budget.
	release := s.admitAuthentication(w, r, s.breakGlassBudget, s.auth.ValidBreakGlassToken(body.Token))
	if release == nil {
		return
	}
	defer release()
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
