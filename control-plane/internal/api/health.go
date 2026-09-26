package api

import (
	"context"
	"net/http"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

func (s *Server) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	diagnostics, ok := s.backend.(database.Diagnostics)
	if !ok {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is not ready", "database dependency is unavailable")
		return
	}
	if err := diagnostics.CheckReadiness(ctx); err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is not ready", "database dependency is unavailable")
		return
	}
	_, platformHub, operationHub := s.eventStreamSnapshots()
	if platformHub.UnhealthyWatchers+operationHub.UnhealthyWatchers > 0 {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/event-stream-unavailable", "Service is not ready", "event stream watcher is recovering")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.build)
}
