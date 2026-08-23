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

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
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
	if s.auth == nil {
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
