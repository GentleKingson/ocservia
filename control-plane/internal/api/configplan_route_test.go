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
