package api

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

type methodSet []string

func (m methodSet) allows(method string) bool { return slices.Contains(m, method) }
func (m methodSet) allow() string             { return strings.Join(m, ", ") }

func (m methodSet) with(method string) methodSet {
	if m.allows(method) {
		return m
	}
	result := append(slices.Clone(m), method)
	slices.Sort(result)
	return result
}

type moduleMethodRule struct {
	path     string
	segments []string
	dynamic  bool
	methods  methodSet
}

// moduleRegistrar is construction-only metadata collection, not a dispatcher.
// The Server retains only its rules; no registrar or handler copies survive.
type moduleRegistrar struct {
	mux   *http.ServeMux
	rules []moduleMethodRule
}

func (r *moduleRegistrar) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	rule, method := parseModulePattern(pattern)
	index := -1
	for i, existing := range r.rules {
		if existing.path == rule.path {
			index = i
			rule.methods = existing.methods
		} else if modulePathsOverlap(existing, rule) {
			panic(fmt.Sprintf("module route %q overlaps %q; keep ambiguous shapes outside the method pilot", pattern, existing.path))
		}
	}
	rule.methods = rule.methods.with(method)
	// ServeMux still rejects invalid names/methods, duplicate registrations and
	// conflicts, including conflicts with legacy registrations. Publish only after
	// it succeeds, so a panic cannot advertise an unregistered method.
	r.mux.HandleFunc(pattern, handler)
	if index >= 0 {
		r.rules[index] = rule
	} else {
		r.rules = append(r.rules, rule)
	}
}

func parseModulePattern(pattern string) (moduleMethodRule, string) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok || method == "" || strings.ContainsAny(method, "\t\r\n") || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "%?#\\ \t\r\n") {
		panic(fmt.Sprintf("unsupported module route pattern %q: require METHOD /path", pattern))
	}
	rule := moduleMethodRule{path: path, segments: strings.Split(path[1:], "/")}
	for _, segment := range rule.segments {
		if segment == "" || segment == "." || segment == ".." {
			panic(fmt.Sprintf("unsupported module route pattern %q: empty or dot segment", pattern))
		}
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			name := segment[1 : len(segment)-1]
			if name == "" || strings.ContainsAny(name, "{}.$") {
				panic(fmt.Sprintf("unsupported module route pattern %q: require whole-segment {name}", pattern))
			}
			rule.dynamic = true
		} else if strings.ContainsAny(segment, "{}") {
			panic(fmt.Sprintf("unsupported module route pattern %q: embedded parameter", pattern))
		}
	}
	return rule, method
}

func modulePathsOverlap(a, b moduleMethodRule) bool {
	if len(a.segments) != len(b.segments) {
		return false
	}
	for i, segment := range a.segments {
		if segment != b.segments[i] && segment[0] != '{' && b.segments[i][0] != '{' {
			return false
		}
	}
	return true
}

func (rule moduleMethodRule) matches(path string) bool {
	if !rule.dynamic {
		return path == rule.path
	}
	// Preserve the old parameter-path Trim/Split contract on URL.Path, not
	// ServeMux's escaped-segment contract. Never clean or decode the request.
	path = strings.Trim(path, "/")
	for _, segment := range rule.segments {
		part, rest, _ := strings.Cut(path, "/")
		if part == "" || segment[0] != '{' && part != segment {
			return false
		}
		path = rest
	}
	return path == ""
}

func moduleRouteMethods(rules []moduleMethodRule, path string) (methodSet, bool) {
	for _, rule := range rules {
		if rule.matches(path) {
			return rule.methods, true
		}
	}
	return nil, false
}

func (s *Server) routeMethods(path string) (methodSet, bool) {
	if methods, ok := moduleRouteMethods(s.moduleMethods, path); ok {
		return methods, true
	}
	method, ok := legacyRouteMethod(path)
	if !ok {
		return nil, false
	}
	return methodSet(strings.Split(method, "_OR_")), true
}

func (s *Server) routeErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods, ok := s.routeMethods(r.URL.Path)
		if !ok {
			writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
			return
		}
		if !methods.allows(r.Method) {
			w.Header().Set("Allow", methods.allow())
			writeProblem(w, r, http.StatusMethodNotAllowed, "https://ocservia.dev/problems/method-not-allowed", "Method not allowed", "the requested method is not supported")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// legacyRouteMethod retains unmigrated routes and value-dependent actions.
func legacyRouteMethod(path string) (string, bool) {
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
	case "/livez", "/readyz", "/version", "/api/v1/livez", "/api/v1/readyz", "/api/v1/version", "/api/v1/operations", "/api/v1/operations/queue-metrics", "/api/v1/operations/summary", "/api/v1/events", "/api/v1/events/stream", "/api/v1/development/runtime", "/api/v1/auth/methods", "/api/v1/auth/callback", "/api/v1/audit/events", "/api/v1/workspaces":
		return http.MethodGet, true
	case "/api/v1/auth/login":
		return "GET_OR_POST", true
	case "/api/v1/development/simulations":
		return http.MethodPost, true
	case "/api/v1/enrollment-tokens", "/api/v1/node-bootstrap-tokens", "/api/v1/auth/logout", "/api/v1/auth/change-password", "/api/v1/auth/break-glass", "/api/v1/approval-requests", "/api/v1/audit:verify", "/api/v1/role-bindings":
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
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "user-group-state" {
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
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "certificates" {
		return "GET_OR_POST", true
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
