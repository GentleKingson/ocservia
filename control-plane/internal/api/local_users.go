package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Server) createLocalUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	s.mutateLocalUser(w, r, auth.LocalUserMutation{Action: "create", Username: body.Username, Password: body.Password})
}

func (s *Server) localUserAction(w http.ResponseWriter, r *http.Request) {
	idText, action, ok := strings.Cut(r.PathValue("local_user_action"), ":")
	id, err := parseUUIDv7(idText)
	if !ok || err != nil || (action != "disable" && action != "reset-password") {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "expected Local identity UUIDv7 and :disable or :reset-password")
		return
	}
	var password string
	if action == "reset-password" {
		var body struct {
			Password string `json:"password"`
		}
		if !decodeStrictJSON(w, r, &body) {
			return
		}
		password = body.Password
	} else {
		var body struct{}
		if !decodeStrictJSON(w, r, &body) {
			return
		}
	}
	s.mutateLocalUser(w, r, auth.LocalUserMutation{Action: action, IdentityID: id, Password: password})
}

func (s *Server) mutateLocalUser(w http.ResponseWriter, r *http.Request, mutation auth.LocalUserMutation) {
	actor := principal(r)
	mutation.ActorID, mutation.SessionID = actor.IdentityID, actor.SessionID
	mutation.WorkspaceID, mutation.RequestID = workspace(r), requestID(r)
	if mutation.Action == "reset-password" {
		mutation.ApprovalID, _ = uuid.Parse(r.Header.Get("X-Approval-ID"))
	}
	id, err := s.auth.MutateLocalUser(r.Context(), mutation)
	if err != nil {
		status, detail := http.StatusInternalServerError, "Local user mutation failed"
		switch {
		case errors.Is(err, approvals.ErrNotReady):
			status, detail = http.StatusConflict, "a matching independent approval is required"
		case errors.Is(err, auth.ErrLocalInvalid):
			status, detail = http.StatusBadRequest, "invalid Local username or password"
		case errors.Is(err, auth.ErrLocalDuplicate):
			status, detail = http.StatusConflict, "Local username already exists"
		case errors.Is(err, pgx.ErrNoRows):
			status, detail = http.StatusNotFound, "Local identity does not exist"
		case errors.Is(err, auth.ErrLocalDisabled):
			status, detail = http.StatusForbidden, "Local authentication is disabled"
		}
		// Never return or log database diagnostics or credential-bearing bodies.
		writeProblem(w, r, status, "https://ocservia.dev/problems/local-user-mutation", "Local user mutation failed", detail)
		return
	}
	if mutation.Action == "create" {
		writeJSON(w, http.StatusCreated, struct {
			IdentityID uuid.UUID `json:"identity_id"`
		}{id})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
