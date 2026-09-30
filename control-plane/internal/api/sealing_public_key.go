package api

import (
	"errors"
	"net/http"

	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
)

func (s *Server) getUserPasswordSealingKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.enrollment == nil {
		writeProblem(w, r, 503, "https://ocservia.dev/problems/sealing-key-unavailable", "Sealing key unavailable", "configure the verified Signer binding before creating or rotating passwords")
		return
	}
	nodeID, ok := pathUUIDv7(r.PathValue("node_id"))
	if !ok {
		writeProblem(w, r, 400, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "node_id must be a UUIDv7")
		return
	}
	key, err := s.enrollment.UserPasswordSealingKey(r.Context(), workspace(r), nodeID)
	if errors.Is(err, enrollment.ErrSealingBinding) {
		writeProblem(w, r, 409, "https://ocservia.dev/problems/sealing-key-binding-mismatch", "Sealing key changed", "verify the current node identity, approved user capability and Signer public key binding before retrying")
		return
	}
	if err != nil {
		writeProblem(w, r, 503, "https://ocservia.dev/problems/sealing-key-unavailable", "Sealing key unavailable", "the verified public key source could not be read; retry after checking Signer availability and binding")
		return
	}
	writeJSON(w, http.StatusOK, key)
}
