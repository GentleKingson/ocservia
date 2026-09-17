package api

import (
	"net/http/httptest"
	"testing"
)

func TestConfigPlanApplyHTTPRoute(t *testing.T) {
	s := baselineServer(t, false)
	path := "/api/v1/config-plans/" + baselineID + "/apply"
	t.Run("authentication", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, baselineRequest("POST", path, nil))
		assertBaselineProblem(t, w, path, 401, "unauthenticated", "Authentication required", "operation state requires an authenticated principal")
		if w.Header().Get("WWW-Authenticate") != "OIDC" || w.Header().Get("Allow") != "" {
			t.Fatalf("authentication headers: %v", w.Header())
		}
	})
	for _, method := range []string{"GET", "PUT", "DELETE", "HEAD", "OPTIONS", "BREW"} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, baselineRequest(method, path, nil))
			assertBaselineProblem(t, w, path, 405, "method-not-allowed", "Method not allowed", "the requested method is not supported")
			if w.Header().Get("Allow") != "POST" || w.Header().Get("WWW-Authenticate") != "" {
				t.Fatalf("method headers: %v", w.Header())
			}
		})
	}
}

func applyHTTPPathBoundaries(id string) []struct {
	path                  string
	status                int
	contentType, location string
} {
	path := "/api/v1/config-plans/" + id + "/apply"
	return []struct {
		path                  string
		status                int
		contentType, location string
	}{
		{path + "-extra", 404, "application/problem+json", ""},
		{path + "/extra", 404, "application/problem+json", ""},
		{"/api/v1/nodes/" + id + "/apply", 404, "application/problem+json", ""},
		{"/api/v1/config-plans-extra/" + id + "/apply", 404, "application/problem+json", ""},
		{"/api/v1/config-plans//apply", 404, "application/problem+json", ""},
		{"/api/v1/config-plans/" + id + "//apply", 404, "application/problem+json", ""},
		{"/api/v1/config-plans/" + id + "%2Fextra/apply", 404, "application/problem+json", ""},
		{"/api/v1/config-plans/" + id + "/%2561pply", 404, "application/problem+json", ""},
		{path + "/", 404, "text/plain; charset=utf-8", ""},
		{"/" + path, 307, "application/problem+json", path},
	}
}

func TestConfigPlanApplyHTTPPathBoundaries(t *testing.T) {
	s := baselineServer(t, false)
	for _, tc := range applyHTTPPathBoundaries(baselineID) {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := baselineRequest("POST", tc.path, nil)
			s.http.Handler.ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Content-Type") != tc.contentType || w.Header().Get("Location") != tc.location || w.Header().Get("WWW-Authenticate") != "" {
				t.Fatalf("boundary: %d %v %s", w.Code, w.Header(), w.Body)
			}
			if tc.status == 404 && tc.contentType == "application/problem+json" {
				assertBaselineProblem(t, w, r.URL.Path, 404, "not-found", "Resource not found", "the requested resource does not exist")
			}
		})
	}
	// UUID validity is not a routing concern, and segment escapes still follow ServeMux.
	for _, id := range []string{"invalid", "%30" + baselineID[1:]} {
		w := httptest.NewRecorder()
		r := baselineRequest("POST", "/api/v1/config-plans/"+id+"/apply", nil)
		s.http.Handler.ServeHTTP(w, r)
		assertBaselineProblem(t, w, r.URL.Path, 401, "unauthenticated", "Authentication required", "operation state requires an authenticated principal")
	}
}
