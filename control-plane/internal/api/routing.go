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

type routeMethodRule struct {
	path     string
	segments []string
	dynamic  bool
	methods  methodSet
}

// methodRegistrar is construction-only metadata collection, not a dispatcher.
// The Server retains only its rules; no registrar or handler copies survive.
type methodRegistrar struct {
	mux   *http.ServeMux
	rules []routeMethodRule
}

func (r *methodRegistrar) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	rule, method := parseRoutePattern(pattern)
	index := -1
	for i, existing := range r.rules {
		if existing.path == rule.path {
			index = i
			rule.methods = existing.methods
		} else if routePathsOverlap(existing, rule) {
			panic(fmt.Sprintf("route %q overlaps %q; keep ambiguous shapes in compatibility rules", pattern, existing.path))
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

func parseRoutePattern(pattern string) (routeMethodRule, string) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok || method == "" || strings.ContainsAny(method, "\t\r\n") || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "%?#\\ \t\r\n") {
		panic(fmt.Sprintf("unsupported route pattern %q: require METHOD /path", pattern))
	}
	rule := routeMethodRule{path: path, segments: strings.Split(path[1:], "/")}
	for _, segment := range rule.segments {
		if segment == "" || segment == "." || segment == ".." {
			panic(fmt.Sprintf("unsupported route pattern %q: empty or dot segment", pattern))
		}
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			name := segment[1 : len(segment)-1]
			if name == "" || strings.ContainsAny(name, "{}.$") {
				panic(fmt.Sprintf("unsupported route pattern %q: require whole-segment {name}", pattern))
			}
			rule.dynamic = true
		} else if strings.ContainsAny(segment, "{}") {
			panic(fmt.Sprintf("unsupported route pattern %q: embedded parameter", pattern))
		}
	}
	return rule, method
}

func routePathsOverlap(a, b routeMethodRule) bool {
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

func (rule routeMethodRule) matches(path string) bool {
	if !rule.dynamic {
		return path == rule.path
	}
	// Preserve the old parameter-path Trim/Split contract on URL.Path, not
	// ServeMux's escaped-segment contract. Never clean or decode the request.
	path = strings.Trim(path, "/")
	last := rule.segments[len(rule.segments)-1]
	if last[0] != '{' && !strings.HasSuffix(path, last) {
		return false
	}
	for _, segment := range rule.segments {
		part, rest, _ := strings.Cut(path, "/")
		if part == "" || segment[0] != '{' && part != segment {
			return false
		}
		path = rest
	}
	return path == ""
}

func registeredRouteMethods(rules []routeMethodRule, path string) (methodSet, bool) {
	// Avoid walking parameter segments when the path cannot have this shape.
	segments := strings.Count(strings.Trim(path, "/"), "/") + 1
	for _, rule := range rules {
		if rule.dynamic && len(rule.segments) != segments {
			continue
		}
		if rule.matches(path) {
			return rule.methods, true
		}
	}
	return nil, false
}

func (s *Server) routeMethods(path string) (methodSet, bool) {
	if methods, ok := registeredRouteMethods(s.registeredMethods, path); ok {
		return methods, true
	}
	method, ok := compatibilityRouteMethod(path)
	if !ok {
		return nil, false
	}
	return methodSet{method}, true
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

// compatibilityRouteMethod retains value-dependent actions and the operation
// detail path's exact prefix/no-slash contract, unlike parameter Trim/Split.
func compatibilityRouteMethod(path string) (string, bool) {
	if strings.HasPrefix(path, "/api/v1/local-users/") {
		id, action, ok := strings.Cut(strings.TrimPrefix(path, "/api/v1/local-users/"), ":")
		if ok && id != "" && !strings.Contains(id, "/") && (action == "disable" || action == "reset-password") {
			return http.MethodPost, true
		}
		return "", false
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "certificates" && parts[3] != "" {
		if strings.HasSuffix(parts[3], ":issue") || strings.HasSuffix(parts[3], ":revoke") || strings.HasSuffix(parts[3], ":p12") {
			return http.MethodPost, true
		}
		return http.MethodGet, true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "secret-provider-refs" && parts[3] != "" {
		if strings.HasSuffix(parts[3], ":rotate") {
			return http.MethodPost, true
		}
		return http.MethodGet, true
	}
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "users" && (strings.HasSuffix(parts[5], ":disable") || strings.HasSuffix(parts[5], ":enable") || strings.HasSuffix(parts[5], ":rotate-password")) {
		return http.MethodPost, true
	}
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "sessions" && (strings.HasSuffix(parts[5], ":disconnect") || strings.HasSuffix(parts[5], ":terminate")) {
		return http.MethodPost, true
	}
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "ip-bans" && strings.HasSuffix(parts[5], ":remove") {
		return http.MethodPost, true
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
