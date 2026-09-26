package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Expand the independent inventory, not the candidate's derived rules. Keep the
// frozen S-02 precheck unchanged and do not spend real authentication budgets twice.
func ordinaryPathCorpus() []string {
	paths := s02PathCorpus()
	for _, line := range strings.Split(routeBaseline, "\n") {
		fields := strings.Split(line, "|")
		_, path := baselineRoutePath(fields[0], fields[1])
		paths = append(paths, path, path+"/", "/"+path, path+"/extra")
		for _, value := range []string{"", ".", "..", "%2E", "%2E%2E", "a%2Fb", "%2F", "%252F", "%252E", "invalid%3Aunknown"} {
			paths = append(paths, strings.ReplaceAll(path, baselineID, value))
		}
		paths = append(paths, strings.ReplaceAll(path, ":", "%3A"), strings.ReplaceAll(path, ":", "%253A"))
		for i, c := range path {
			if c == '/' {
				paths = append(paths, path[:i]+"/"+path[i:], path[:i]+"/."+path[i:], path[:i]+"/%2E"+path[i:], path[:i]+"/.."+path[i:])
			}
		}
	}
	return paths
}

func TestOrdinaryMethodCompatibility(t *testing.T) {
	s := baselineServer(t, false)
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Precheck-Passed", "true")
		fmt.Fprintf(w, "%s %s %s", r.Method, r.URL.Path, r.URL.RawPath)
	})
	candidate := s.requestContext(s.routeErrors(probe))
	reference := s.requestContext(s02BaselineRouteErrors(probe))
	methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "BREW", "get"}
	paths := ordinaryPathCorpus()
	t.Logf("S-03 base=b4f0f78c0cf13d87c4e0f3b4e046e725892bca5e paths=%d requests=%d", len(paths), len(paths)*len(methods))
	for _, path := range paths {
		for _, method := range methods {
			r := baselineRequest(method, path, nil)
			want, wantOK := s02BaselineRouteMethod(r.URL.Path)
			got, gotOK := s.routeMethods(r.URL.Path)
			if got.allow() != strings.ReplaceAll(want, "_OR_", ", ") || gotOK != wantOK {
				t.Fatalf("%s: methods=%q/%v baseline=%q/%v", path, got, gotOK, want, wantOK)
			}
			w, old := httptest.NewRecorder(), httptest.NewRecorder()
			candidate.ServeHTTP(w, r)
			reference.ServeHTTP(old, baselineRequest(method, path, nil))
			if w.Code != old.Code || !reflect.DeepEqual(w.Header(), old.Header()) || w.Body.String() != old.Body.String() {
				t.Fatalf("%s %s: response=%d %v %q baseline=%d %v %q", method, path, w.Code, w.Header(), w.Body.String(), old.Code, old.Header(), old.Body.String())
			}
		}
	}
}

func TestOrdinaryHTTPPathBoundaries(t *testing.T) {
	s := baselineServer(t, false)
	for _, tc := range []struct {
		method, path, allow, kind string
		status                    int
	}{
		{"GET", "/api/v1/operations/summary/", "", "not-found", 404},
		{"GET", "//api/v1/operations/summary", "", "not-found", 404},
		{"GET", "/api/v1/operations/invalid/", "", "not-found", 404},
		{"GET", "/api/v1/operations/a%2Fb", "", "not-found", 404},
		{"GET", "/api/v1/operations/a%252Fb", "", "unauthenticated", 401},
		{"GET", "/api/v1/operations/%73ummary", "", "unauthenticated", 401},
		{"POST", "/api/v1/operations/summary", "GET", "method-not-allowed", 405},
		{"POST", "/api/v1/operations/queue-metrics", "GET", "method-not-allowed", 405},
		{"HEAD", "/api/v1/auth/login", "GET, POST", "method-not-allowed", 405},
		{"POST", "/api/v1/auth/login/", "", "not-found", 404},
		{"POST", "/api/v1/auth/./login", "", "not-found", 404},
		{"POST", "/api/v1/auth/%2E/login", "", "not-found", 404},
		{"POST", "/api/v1/auth//login", "", "not-found", 404},
		{"GET", "/api/v1/agent-rollouts/", "", "not-found", 404},
		{"DELETE", "/api/v1/agent-rollouts", "GET, POST", "method-not-allowed", 405},
		{"PUT", "/api/v1/nodes/invalid/certificates", "GET, POST", "method-not-allowed", 405},
		{"POST", "/api/v1/certificates/invalid%3Ap12", "", "unauthenticated", 401},
		{"POST", "/api/v1/certificates/invalid%253Ap12", "GET", "method-not-allowed", 405},
		{"GET", "/api/v1/certificates/invalid:issue", "POST", "method-not-allowed", 405},
		{"POST", "/api/v1/secret-provider-refs/invalid:rotate", "", "unauthenticated", 401},
		{"POST", "/api/v1/secret-provider-refs/invalid:unknown", "GET", "method-not-allowed", 405},
		{"GET", "/api/v1/approval-requests/invalid:approve", "POST", "method-not-allowed", 405},
		{"POST", "/api/v1/local-users/invalid:reset-password", "", "unauthenticated", 401},
		{"POST", "/api/v1/local-users/invalid:reset-password/", "", "not-found", 404},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := baselineRequest(tc.method, tc.path, strings.NewReader("{"))
			s.http.Handler.ServeHTTP(w, r)
			title, detail, challenge := "Resource not found", "the requested resource does not exist", ""
			if tc.status == 401 {
				title, detail, challenge = "Authentication required", "operation state requires an authenticated principal", "OIDC"
			} else if tc.status == 405 {
				title, detail = "Method not allowed", "the requested method is not supported"
			}
			assertBaselineProblem(t, w, r.URL.Path, tc.status, tc.kind, title, detail)
			if w.Header().Get("Allow") != tc.allow || w.Header().Get("WWW-Authenticate") != challenge || w.Header().Get("Location") != "" {
				t.Fatal("unexpected response headers", w.Header())
			}
		})
	}
	for _, tc := range []struct {
		path, location, body string
		status               int
	}{
		{"/api/v1/agent-rollouts/invalid/", "", "404 page not found\n", 404},
		{"/api/v1/artifacts/invalid/", "", "404 page not found\n", 404},
		{"//api/v1/artifacts/invalid", "/api/v1/artifacts/invalid", "<a href=\"/api/v1/artifacts/invalid\">Temporary Redirect</a>.\n\n", 307},
	} {
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, baselineRequest("GET", tc.path, nil))
		contentType := "text/plain; charset=utf-8"
		if tc.status == 307 {
			contentType = "text/html; charset=utf-8"
		}
		if w.Code != tc.status || w.Header().Get("Location") != tc.location || w.Header().Get("Content-Type") != contentType || w.Body.String() != tc.body || w.Header().Get("WWW-Authenticate") != "" {
			t.Fatalf("%s: %d %v %q", tc.path, w.Code, w.Header(), w.Body.String())
		}
	}
}

func BenchmarkRouteMethods(b *testing.B) {
	s := NewBackend("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "")
	b.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	for _, tc := range []struct{ name, path string }{
		{"ordinary-static", "/api/v1/auth/login"},
		{"ordinary-parameter", "/api/v1/nodes/" + baselineID + "/certificates"},
		{"compatibility-action", "/api/v1/certificates/" + baselineID + ":p12"},
		{"compatibility-operation", "/api/v1/operations/" + baselineID},
		{"miss", "/api/v1/missing/" + baselineID},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				s.routeMethods(tc.path)
			}
		})
	}
}
