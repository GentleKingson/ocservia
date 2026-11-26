package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/devsim"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
)

// allowDevelopmentRequest admits development routes only for the development
// principal addressed through a loopback Host or the development Compose web
// service, whose Vite proxy forwards the browser Host and enforces the same
// allowedHosts list. The default development token is public, so the Host
// check keeps a DNS-rebound browser page from reaching these routes.
func (s *Server) allowDevelopmentRequest(w http.ResponseWriter, r *http.Request) bool {
	if !s.hasOperationCredential(r) {
		writeProblem(w, r, http.StatusUnauthorized, "https://ocservia.dev/problems/unauthenticated", "Authentication required", "development routes require the development principal")
		return false
	}
	if !developmentHost(r.Host) {
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Access denied", "development routes require a loopback Host")
		return false
	}
	return true
}

// developmentHost reports whether a request Host is one the development
// principal may be addressed through: localhost, a loopback IP, or the
// development Compose web service.
func developmentHost(hostport string) bool {
	host := hostport
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return host == "localhost" || host == "web" || (ip != nil && ip.IsLoopback())
}

func (s *Server) createSimulation(w http.ResponseWriter, r *http.Request) {
	service := s.devSim
	if service == nil || !s.localSimulator {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	if !s.allowDevelopmentRequest(w, r) {
		return
	}
	var scenario *devsim.SimulationScenario
	if !decodeStrictJSON(w, r, &scenario) {
		return
	}
	if scenario == nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the simulation request must be a JSON object")
		return
	}
	operation, err := service.CreateSimulation(r.Context(), *scenario, requestID(r), requestTraceparent(r))
	if err != nil {
		s.logger.ErrorContext(r.Context(), "create local simulation", "error", err)
		if errors.Is(err, devsim.ErrInvalidScenario) {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-simulation", "Simulation is invalid", "the simulation could not be accepted")
			return
		}
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "the simulation could not be persisted")
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) developmentRuntime(w http.ResponseWriter, r *http.Request) {
	if !s.localSimulator {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	if !s.allowDevelopmentRequest(w, r) {
		return
	}
	var pool database.PoolStats
	if s.backend != nil {
		pool = s.backend.PoolStats()
	}
	admission, platformHub, operationHub := s.eventStreamSnapshots()
	metricsContext, cancelMetrics := context.WithTimeout(r.Context(), time.Second)
	defer cancelMetrics()
	keyStates, err := privdattestation.KeyStateMetrics(metricsContext, s.backend)
	keyStatesAvailable := s.backend != nil && err == nil
	if err != nil {
		// Keep process and SSE diagnostics available while the database is down.
		// The availability bit prevents the bounded zero-value series from being
		// mistaken for a successful database observation.
		keyStates, _ = privdattestation.KeyStateMetrics(r.Context(), nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"goroutines":                             runtime.NumGoroutine(),
		"db_acquired":                            pool.Acquired,
		"db_idle":                                pool.Idle,
		"db_total":                               pool.Total,
		"sse_active_streams":                     admission.Active,
		"sse_rejected_streams":                   admission.RejectedGlobal + admission.RejectedIdentity + admission.RejectedSession + admission.RejectedWorkspace + admission.RejectedResource,
		"sse_watchers":                           platformHub.Watchers + operationHub.Watchers,
		"sse_unhealthy_watchers":                 platformHub.UnhealthyWatchers + operationHub.UnhealthyWatchers,
		"sse_sql_queries":                        platformHub.Queries + operationHub.Queries,
		"sse_slow_consumer_disconnects":          platformHub.SlowConsumerDisconnects + operationHub.SlowConsumerDisconnects,
		"sse_database_backoff_seconds":           (platformHub.DatabaseBackoff + operationHub.DatabaseBackoff).Seconds(),
		"privd_receipt_verifications":            privdattestation.VerificationMetrics(),
		"privd_attestation_key_states":           keyStates,
		"privd_attestation_key_states_available": keyStatesAvailable,
	})
}
