package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// This corpus is independent of the registrar and includes both pilot and
// adjacent legacy paths. It runs unchanged before and after replacing rules.
func s02PathCorpus() []string {
	paths := []string{
		"/api/v1/nodes", "/api/v1/nodes/invalid",
		"/api/v1/nodes/invalid/sessions", "/api/v1/nodes/invalid/ip-bans",
		"/api/v1/nodes/invalid/telemetry", "/api/v1/nodes/invalid/config-plans",
		"/api/v1/config-plans/invalid", "/api/v1/config-plans/invalid/apply",
		"/api/v1/nodes/invalid/users/alice/policy", "/api/v1/user-batches",
		"/api/v1/user-batches/invalid", "/api/v1/user-operations/metrics",
		"/api/v1/nodes/invalid/user-group-state", "/api/v1/nodes/invalid/users",
		"/api/v1/nodes/invalid/groups/operators", "/api/v1/nodes/invalid/certificates",
		"/api/v1/enrollment-tokens", "/api/v1/node-bootstrap-tokens",
		"/api/v1/nodes/invalid/approval", "/api/v1/nodes/invalid/revocation",
		"/api/v1/nodes/invalid/privd-attestation-credentials",
		"/api/v1/nodes/invalid/privd-attestation-keys:revoke",
		"/api/v1/nodes/invalid/does-not-exist", "/missing",
	}
	for _, action := range []string{"unknown", "disconnect", "terminate", "remove", "disable", "enable", "rotate-password", "issue", "revoke", "p12", "rotate", "approve"} {
		for _, prefix := range []string{
			"/api/v1/nodes/invalid/sessions/42:", "/api/v1/nodes/invalid/ip-bans/192.0.2.9:",
			"/api/v1/nodes/invalid/users/alice:", "/api/v1/certificates/invalid:",
			"/api/v1/secret-provider-refs/invalid:", "/api/v1/local-users/invalid:",
			"/api/v1/approval-requests/invalid:",
		} {
			paths = append(paths, prefix+action)
		}
	}
	var corpus []string
	for _, path := range paths {
		corpus = append(corpus, path, path+"/", "/"+path, path+"/extra")
		for _, value := range []string{"", ".", "..", "a%2Fb", "a%252Fb", "%2E", "%2F", "%252E", "%25", "invalid%3Aunknown"} {
			corpus = append(corpus, strings.ReplaceAll(path, "invalid", value))
		}
		corpus = append(corpus, strings.Replace(path, "/api/v1/", "/api//v1/", 1), strings.Replace(path, "/api/v1/", "/api/v1/./", 1))
		corpus = append(corpus, strings.Replace(path, "/api/v1/", "/api/v1/../v1/", 1), strings.Replace(path, "/api/", "/%61pi/", 1))
		corpus = append(corpus, strings.ReplaceAll(path, ":", "%3A"), strings.ReplaceAll(path, ":", "%253A"))
	}
	return append(corpus, "/api/v1/%6eodes/invalid", "/api/v1/%256eodes/invalid", "/api/v1/config-plans/invalid/%61pply", "/api/v1/config-plans/invalid/%2561pply", "/api/v1/nodes/invalid/users/a%2Fb/policy")
}

func TestModuleMethodCompatibility(t *testing.T) {
	s := baselineServer(t, false)
	// Only the old precheck is frozen. The same real declarations, root mux,
	// guards and middleware isolate method-source changes from HTTP behavior.
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	reference := s.requestContext(s.limitBody(s.timeout(s02BaselineRouteErrors(mux))))
	methods := []string{"GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS", "BREW", "get"}
	corpus := s02PathCorpus()
	t.Logf("baseline=e41b9a78e74c5920fe4741061800c1b8e8f56dc1 paths=%d requests=%d", len(corpus), len(corpus)*len(methods))
	for _, path := range corpus {
		for _, method := range methods {
			r := baselineRequest(method, path, strings.NewReader("broken JSON"))
			want, wantOK := s02BaselineRouteMethod(r.URL.Path)
			got, gotOK := routeMethod(r.URL.Path)
			if got != want || gotOK != wantOK {
				t.Fatalf("%s: rule=%q/%v, baseline=%q/%v", path, got, gotOK, want, wantOK)
			}
			w, old := httptest.NewRecorder(), httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, r)
			reference.ServeHTTP(old, baselineRequest(method, path, strings.NewReader("broken JSON")))
			if w.Code != old.Code || !reflect.DeepEqual(w.Header(), old.Header()) || w.Body.String() != old.Body.String() {
				t.Fatalf("%s %s:\nresponse=%d %v %q\nbaseline=%d %v %q", method, path, w.Code, w.Header(), w.Body.String(), old.Code, old.Header(), old.Body.String())
			}
		}
	}
}
