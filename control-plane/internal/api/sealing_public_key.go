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
	nodeID, ok := requirePathUUIDv7(w, r, "node_id", "")
	if !ok {
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
