package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"go.opentelemetry.io/otel/trace"
)

const baselineID = "019fc0a4-6d92-765c-a8a1-4af556614cc3"

func baselineServer(t *testing.T, dev bool) *Server {
	t.Helper()
	s, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: dev, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{Version: "baseline", Commit: "fixture"}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}

func baselineRequest(method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	r.Host = "127.0.0.1:8080"
	r.Header.Set("X-Request-ID", "baseline-request")
	tid, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	sid, _ := trace.SpanIDFromHex("0123456789abcdef")
	return r.WithContext(trace.ContextWithSpanContext(r.Context(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid})))
}

func assertBaselineProblem(t *testing.T, w *httptest.ResponseRecorder, path string, status int, kind, title, detail string) {
	t.Helper()
	if w.Code != status || w.Header().Get("Content-Type") != "application/problem+json" || w.Header().Get("X-Request-ID") != "baseline-request" {
		t.Fatalf("response: %d %v %s", w.Code, w.Header(), w.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"type": "https://ocservia.dev/problems/" + kind, "title": title, "detail": detail, "status": float64(status), "instance": path, "trace_id": "0123456789abcdef0123456789abcdef"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Problem = %#v, want %#v", got, want)
	}
}

func parseOpenAPIPaths(t *testing.T, specPath string) map[string][]string {
	t.Helper()
	file, err := os.Open(specPath)
	if err != nil {
		t.Fatalf("open OpenAPI spec %s: %v", specPath, err)
	}
	defer file.Close()

	paths := map[string][]string{}
	inPaths := false
	currentPath := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			inPaths = line == "paths:"
			currentPath = ""
			continue
		}
		if !inPaths {
			continue
		}
		if strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":") && !strings.HasPrefix(line, "   ") {
			currentPath = strings.TrimSuffix(strings.TrimPrefix(line, "  "), ":")
			continue
		}
		if currentPath != "" && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") {
			trimmed := strings.TrimSpace(line)
			for _, method := range []string{"get:", "post:", "put:", "delete:", "patch:"} {
				if trimmed == method {
					upper := strings.ToUpper(strings.TrimSuffix(method, ":"))
					paths[currentPath] = append(paths[currentPath], upper)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan OpenAPI spec: %v", err)
	}
	for path, methods := range paths {
		slices.Sort(methods)
		paths[path] = methods
	}
	return paths
}

func materializeOpenAPIPath(path string) string {
	path = strings.NewReplacer(
		"{group_name}", "operators",
		"{username}", "alice",
		"{session_id}", "42",
		"{ip}", "192.0.2.9",
	).Replace(path)
	for strings.Contains(path, "{") {
		start, end := strings.Index(path, "{"), strings.Index(path, "}")
		path = path[:start] + baselineID + path[end+1:]
	}
	return path
}

func normalizeRouteShape(path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for i, seg := range segments {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			segments[i] = "{}"
		}
	}
	return "/" + strings.Join(segments, "/")
}

func openAPIExpectedRoutes(t *testing.T) map[string][]string {
	t.Helper()
	specPath := filepath.Join("..", "..", "..", "openapi", "openapi.yaml")
	specPaths := parseOpenAPIPaths(t, specPath)
	if len(specPaths) == 0 {
		t.Fatal("parsed zero OpenAPI paths")
	}
	expected := map[string][]string{
		"/livez":   {"GET"},
		"/readyz":  {"GET"},
		"/version": {"GET"},
	}
	for relPath, methods := range specPaths {
		expected["/api/v1"+relPath] = methods
	}
	return expected
}

// TestOpenAPIRouteConformance verifies the live HTTP router against openapi.yaml
// without pinning internal Go handler names, AST layouts, or historical counts.
func TestOpenAPIRouteConformance(t *testing.T) {
	expectedMethods := openAPIExpectedRoutes(t)
	s := baselineServer(t, false)

	// Endpoints intentionally exempt from requireOperationAuth (public health
	// probes, self-authenticated login/break-glass/installation, dev simulator).
	unauthenticatedBehavior := map[string]string{
		"/livez":                             "live",
		"/readyz":                            "ready",
		"/version":                           "version",
		"/api/v1/livez":                      "live",
		"/api/v1/readyz":                     "ready",
		"/api/v1/version":                    "version",
		"/api/v1/auth/login":                 "Local authentication is not configured",
		"/api/v1/auth/break-glass":           "break-glass is not configured",
		"/api/v1/development/simulations":    "the requested resource does not exist",
		"/api/v1/development/runtime":        "the requested resource does not exist",
		"/api/v1/node-bootstrap/config":      "node installation is not enabled",
		"/api/v1/node-bootstrap/attestation": "node installation is not enabled",
		"/api/v1/node-bootstrap/register":    "node installation is not enabled",
		"/api/v1/node-bootstrap/status":      "node installation is not enabled",
	}

	specShapes := make(map[string]bool, len(expectedMethods))
	for full := range expectedMethods {
		specShapes[normalizeRouteShape(full)] = true
	}

	// 1. Every registered non-compat route on the server must exist in OpenAPI (or root health aliases).
	routes := &methodRegistrar{mux: http.NewServeMux(), paths: make(map[string]map[string]http.HandlerFunc)}
	s.registerRoutes(routes)
	for path := range routes.paths {
		shape := normalizeRouteShape(path)
		if !specShapes[shape] {
			t.Errorf("live registered route %s is undocumented in openapi/openapi.yaml", path)
		}
	}

	// 2. Every OpenAPI path+method (and root alias) must be routed with exact allowed methods,
	//    proper auth fail-closed behavior, and 405 Method Not Allowed with Allow header.
	for pattern, wantMethods := range expectedMethods {
		t.Run(pattern, func(t *testing.T) {
			path := materializeOpenAPIPath(pattern)
			wantAllow := strings.Join(wantMethods, ", ")
			for _, method := range wantMethods {
				w := httptest.NewRecorder()
				s.http.Handler.ServeHTTP(w, baselineRequest(method, path, nil))
				if behavior, exempt := unauthenticatedBehavior[pattern]; exempt {
					switch behavior {
					case "live":
						if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || w.Body.String() != "{\"status\":\"ok\"}\n" {
							t.Fatalf("live: %d %s", w.Code, w.Body)
						}
					case "version":
						if w.Code != 200 || w.Body.String() != "{\"version\":\"baseline\",\"commit\":\"fixture\",\"role\":\"all\"}\n" {
							t.Fatalf("version: %d %s", w.Code, w.Body)
						}
					case "ready":
						assertBaselineProblem(t, w, path, 503, "database-unavailable", "Service is not ready", "database dependency is unavailable")
					default:
						assertBaselineProblem(t, w, path, 404, "not-found", "Resource not found", behavior)
					}
				} else {
					assertBaselineProblem(t, w, path, 401, "unauthenticated", "Authentication required", "operation state requires an authenticated principal")
				}
			}
			for _, wrong := range []string{"HEAD", "OPTIONS", "BREW"} {
				w := httptest.NewRecorder()
				s.http.Handler.ServeHTTP(w, baselineRequest(wrong, path, nil))
				assertBaselineProblem(t, w, path, 405, "method-not-allowed", "Method not allowed", "the requested method is not supported")
				if w.Header().Get("Allow") != wantAllow || w.Header().Get("WWW-Authenticate") != "" {
					t.Fatalf("method headers: %v", w.Header())
				}
			}
		})
	}
}
