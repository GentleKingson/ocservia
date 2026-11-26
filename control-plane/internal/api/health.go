package api

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if s.backend == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is not ready", "database dependency is unavailable")
		return
	}
	if err := s.backend.CheckReadiness(ctx); err != nil {
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
	w.Header().Set("Cache-Control", "private, max-age=60")
	// Keep the historical response field without making it a runtime setting.
	writeJSON(w, http.StatusOK, struct {
		Version                 string `json:"version"`
		Commit                  string `json:"commit"`
		Role                    string `json:"role"`
		RecommendedAgentVersion string `json:"recommended_agent_version,omitempty"`
	}{s.build.Version, s.build.Commit, "all", s.build.RecommendedAgentVersion})
}
