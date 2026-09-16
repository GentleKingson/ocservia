package api

import (
	"net/http"
	"strings"
)

func (s *Server) routeErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expectedMethod, ok := routeMethod(r.URL.Path)
		if !ok {
			writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
			return
		}
		methodAllowed := r.Method == expectedMethod || expectedMethod == "GET_OR_PUT" && (r.Method == http.MethodGet || r.Method == http.MethodPut) || expectedMethod == "GET_OR_POST" && (r.Method == http.MethodGet || r.Method == http.MethodPost)
		if !methodAllowed {
			allow := expectedMethod
			if expectedMethod == "GET_OR_PUT" {
				allow = "GET, PUT"
			} else if expectedMethod == "GET_OR_POST" {
				allow = "GET, POST"
			}
			w.Header().Set("Allow", allow)
			writeProblem(w, r, http.StatusMethodNotAllowed, "https://ocservia.dev/problems/method-not-allowed", "Method not allowed", "the requested method is not supported")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func routeMethod(path string) (string, bool) {
	if path == "/api/v1/local-users" {
		return http.MethodPost, true
	}
	if strings.HasPrefix(path, "/api/v1/local-users/") {
		id, action, ok := strings.Cut(strings.TrimPrefix(path, "/api/v1/local-users/"), ":")
		if ok && id != "" && !strings.Contains(id, "/") && (action == "disable" || action == "reset-password") {
			return http.MethodPost, true
		}
		return "", false
	}
	switch path {
	case "/livez", "/readyz", "/version", "/api/v1/livez", "/api/v1/readyz", "/api/v1/version", "/api/v1/operations", "/api/v1/operations/queue-metrics", "/api/v1/operations/summary", "/api/v1/user-operations/metrics", "/api/v1/events", "/api/v1/events/stream", "/api/v1/development/runtime", "/api/v1/auth/methods", "/api/v1/auth/callback", "/api/v1/audit/events", "/api/v1/workspaces":
		return http.MethodGet, true
	case "/api/v1/auth/login":
		return "GET_OR_POST", true
	case "/api/v1/nodes":
		return http.MethodGet, true
	case "/api/v1/development/simulations":
		return http.MethodPost, true
	case "/api/v1/enrollment-tokens", "/api/v1/node-bootstrap-tokens", "/api/v1/auth/logout", "/api/v1/auth/change-password", "/api/v1/auth/break-glass", "/api/v1/approval-requests", "/api/v1/audit:verify", "/api/v1/role-bindings", "/api/v1/user-batches":
		return http.MethodPost, true
	}
	if path == "/api/v1/secret-provider-refs" {
		return http.MethodPost, true
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "certificates" && parts[3] != "" {
		if strings.HasSuffix(parts[3], ":issue") || strings.HasSuffix(parts[3], ":revoke") || strings.HasSuffix(parts[3], ":p12") {
			return http.MethodPost, true
		}
		return http.MethodGet, true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "artifacts" && parts[3] != "" {
		return http.MethodGet, true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "secret-provider-refs" && parts[3] != "" {
		if strings.HasSuffix(parts[3], ":rotate") {
			return http.MethodPost, true
		}
		return http.MethodGet, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && (parts[4] == "approval" || parts[4] == "revocation") {
		return http.MethodPost, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && (parts[4] == "privd-attestation-credentials" || parts[4] == "privd-attestation-keys:register" || parts[4] == "privd-attestation-keys:revoke") {
		return http.MethodPost, true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" {
		return http.MethodGet, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && (parts[4] == "sessions" || parts[4] == "telemetry" || parts[4] == "ip-bans" || parts[4] == "user-group-state") {
		return http.MethodGet, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "users" {
		return http.MethodPost, true
	}
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "users" && (strings.HasSuffix(parts[5], ":disable") || strings.HasSuffix(parts[5], ":enable") || strings.HasSuffix(parts[5], ":rotate-password")) {
		return http.MethodPost, true
	}
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "groups" && parts[5] != "" {
		return http.MethodPut, true
	}
	if len(parts) == 7 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "users" && parts[5] != "" && parts[6] == "policy" {
		return "GET_OR_PUT", true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "user-batches" && parts[3] != "" {
		return http.MethodGet, true
	}
	if path == "/api/v1/agent-rollouts" {
		return "GET_OR_POST", true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "agent-rollouts" && parts[3] != "" {
		return http.MethodGet, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "agent-rollouts" && parts[3] != "" && parts[4] == "resume" {
		return http.MethodPost, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "synthetic-commands" {
		return http.MethodPost, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "config-plans" {
		return http.MethodPost, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "certificates" {
		return "GET_OR_POST", true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "config-plans" && parts[3] != "" {
		return http.MethodGet, true
	}
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "sessions" && (strings.HasSuffix(parts[5], ":disconnect") || strings.HasSuffix(parts[5], ":terminate")) {
		return http.MethodPost, true
	}
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "ip-bans" && strings.HasSuffix(parts[5], ":remove") {
		return http.MethodPost, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "service:reload" {
		return http.MethodPost, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "agent-upgrade" {
		return http.MethodPost, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "operations" && parts[3] != "" && parts[4] == "events" {
		return http.MethodGet, true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "approval-requests" && strings.HasSuffix(parts[3], ":approve") {
		return http.MethodPost, true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "approval-requests" && parts[3] != "" {
		return http.MethodGet, true
	}
	operationID := strings.TrimPrefix(path, "/api/v1/operations/")
	if operationID != path && operationID != "" && !strings.Contains(operationID, "/") {
		return http.MethodGet, true
	}
	return "", false
}
