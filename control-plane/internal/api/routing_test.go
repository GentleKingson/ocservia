package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/api/useroperationshttp"
)

func TestModuleMethodRegistration(t *testing.T) {
	mux := http.NewServeMux()
	registrar := &moduleRegistrar{mux: mux}
	s := &Server{}
	calls := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.PathValue("item") != "a%2Fb" || r.Method != "GET" || r.URL.Path != "/s02/a%2Fb" || r.URL.RawPath != "" {
			t.Errorf("request or parameter changed: %s %v %q", r.Method, r.URL, r.PathValue("item"))
		}
		w.WriteHeader(204)
	}
	registrar.HandleFunc("GET /s02/{item}", handler)
	s.moduleMethods = registrar.rules
	if _, ok := legacyRouteMethod("/s02/a"); ok {
		t.Fatal("test route must not be known to the legacy table")
	}
	if methods, ok := s.routeMethods("/s02/a"); !ok || methods.allow() != "GET" {
		t.Fatalf("single registration did not derive methods: %v %v", methods, ok)
	}
	w := httptest.NewRecorder()
	s.routeErrors(mux).ServeHTTP(w, httptest.NewRequest("GET", "/s02/a%252Fb", nil))
	if w.Code != 204 || calls != 1 {
		t.Fatalf("root mux dispatch: %d calls=%d", w.Code, calls)
	}
	w = httptest.NewRecorder()
	s.routeErrors(mux).ServeHTTP(w, httptest.NewRequest("PUT", "/s02/a", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "GET" || calls != 1 {
		t.Fatalf("wrong method reached handler: %d %v calls=%d", w.Code, w.Header(), calls)
	}
	puts := 0
	registrar.HandleFunc("PUT /s02/{item}", func(w http.ResponseWriter, r *http.Request) {
		puts++
		if r.PathValue("item") != "a" {
			t.Error("root mux did not bind parameter")
		}
		w.WriteHeader(202)
	})
	s.moduleMethods = registrar.rules
	if methods, ok := s.routeMethods("/s02/a"); !ok || methods.allow() != "GET, PUT" {
		t.Fatalf("second registration did not merge methods: %v %v", methods, ok)
	}
	w = httptest.NewRecorder()
	s.routeErrors(mux).ServeHTTP(w, httptest.NewRequest("PUT", "/s02/a", nil))
	if w.Code != 202 || puts != 1 || calls != 1 {
		t.Fatalf("second handler dispatch: %d GET=%d PUT=%d", w.Code, calls, puts)
	}
	for _, method := range []string{"POST", "HEAD", "OPTIONS", "BREW"} {
		w := httptest.NewRecorder()
		s.routeErrors(mux).ServeHTTP(w, httptest.NewRequest(method, "/s02/a", nil))
		if w.Code != 405 || w.Header().Get("Allow") != "GET, PUT" || puts != 1 || calls != 1 {
			t.Fatalf("method %s: %d %v calls=%d/%d", method, w.Code, w.Header(), calls, puts)
		}
	}
}

func TestModuleMethodRegistrationFailure(t *testing.T) {
	handler := func(http.ResponseWriter, *http.Request) {}
	for _, pattern := range []string{
		"", "/missing-method", "GET example.test/path", "GET /", "GET /subtree/",
		"GET /a//b", "GET /a/./b", "GET /a/../b", "GET /a/{tail...}",
		"GET /a/{$}", "GET /a/pre-{name}", "GET /a/{name}:action", "GET /a/{}",
		"GET /a/{bad-name}", "GET /a/{name}/{name}", "GET /a/{unterminated",
		"GET /a/%61", "GET /a?query", "GET /a#fragment", "GET /a\\b", "GET  /a",
		"BAD(METHOD /a", "GET\t /a", "GET /a\nb",
	} {
		t.Run(fmt.Sprintf("invalid-%q", pattern), func(t *testing.T) {
			registrar := &moduleRegistrar{mux: http.NewServeMux()}
			mustRegistrationPanic(t, func() { registrar.HandleFunc(pattern, handler) })
			if len(registrar.rules) != 0 {
				t.Fatal("failed construction published metadata", registrar.rules)
			}
		})
	}
	for _, pattern := range []string{"GET /s02/{id}", "POST /s02/fixed", "POST /s02/{other}"} {
		t.Run("conflict-"+pattern, func(t *testing.T) {
			mux := http.NewServeMux()
			registrar := &moduleRegistrar{mux: mux}
			registrar.HandleFunc("GET /s02/{id}", handler)
			before := slices.Clone(registrar.rules)
			mustRegistrationPanic(t, func() { registrar.HandleFunc(pattern, handler) })
			if !reflect.DeepEqual(before, registrar.rules) {
				t.Fatal("failed registration changed existing rules")
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("POST", "/s02/fixed", nil))
			if w.Code != 405 {
				t.Fatal("rejected overlapping handler was installed", w.Code)
			}
		})
	}
	t.Run("root-mux-conflict", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /s02/{id}", handler)
		registrar := &moduleRegistrar{mux: mux}
		mustRegistrationPanic(t, func() { registrar.HandleFunc("GET /s02/{id}", handler) })
		if len(registrar.rules) != 0 {
			t.Fatal("root mux failure published metadata")
		}
	})
	t.Run("nil-handler", func(t *testing.T) {
		mux := http.NewServeMux()
		registrar := &moduleRegistrar{mux: mux}
		mustRegistrationPanic(t, func() { registrar.HandleFunc("GET /s02", nil) })
		if len(registrar.rules) != 0 {
			t.Fatal("nil handler published metadata")
		}
		registrar.HandleFunc("GET /s02", handler)
	})
}

func mustRegistrationPanic(t *testing.T, register func()) {
	t.Helper()
	defer func() {
		if failure := recover(); failure == nil || fmt.Sprint(failure) == "" {
			t.Error("invalid registration did not fail with a diagnostic")
		}
	}()
	register()
}

func TestModuleMethodPrecedence(t *testing.T) {
	// The legacy rule would allow GET. A recognized pilot path must terminate
	// lookup even when that would have allowed the request's method.
	mux := http.NewServeMux()
	registrar := &moduleRegistrar{mux: mux}
	calls := 0
	registrar.HandleFunc("PUT /api/v1/nodes/{id}/user-group-state", func(http.ResponseWriter, *http.Request) { calls++ })
	s := &Server{moduleMethods: registrar.rules}
	w := httptest.NewRecorder()
	s.routeErrors(mux).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/nodes/a/user-group-state", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "PUT" || calls != 0 {
		t.Fatalf("pilot rejection fell back to legacy: %d %v calls=%d", w.Code, w.Header(), calls)
	}
	production := baselineServer(t, false)
	path := "/api/v1/nodes/a/user-group-state"
	if _, ok := moduleRouteMethods(production.moduleMethods, path); ok {
		t.Fatal("production pilot took over unmigrated user-group-state")
	}
	if methods, ok := production.routeMethods(path); !ok || methods.allow() != "GET" {
		t.Fatal("legacy user-group-state lost", methods, ok)
	}
}

func TestModuleMethodIsolationAndOrder(t *testing.T) {
	s, other := baselineServer(t, false), baselineServer(t, false)
	if &s.moduleMethods[0] == &other.moduleMethods[0] || &s.moduleMethods[0].methods[0] == &other.moduleMethods[0].methods[0] {
		t.Fatal("Servers share method metadata")
	}
	// Reorder actual module registrations, not a duplicate test route table.
	registrations := []func(*moduleRegistrar){
		func(r *moduleRegistrar) { s.nodeHTTP.Register(r, s.requireActionAuth) },
		func(r *moduleRegistrar) { s.configPlanHTTP.Register(r, s.requireActionAuth) },
		func(r *moduleRegistrar) { s.userOpsHTTP.Register(r, s.requireActionAuth) },
	}
	for _, order := range [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		registrar := &moduleRegistrar{mux: http.NewServeMux()}
		for _, i := range order {
			registrations[i](registrar)
		}
		for _, path := range s02PathCorpus() {
			path = httptest.NewRequest("GET", path, nil).URL.Path
			want, wantOK := moduleRouteMethods(s.moduleMethods, path)
			got, ok := moduleRouteMethods(registrar.rules, path)
			if !slices.Equal(got, want) || ok != wantOK {
				t.Fatalf("order=%v path=%s: %v/%v != %v/%v", order, path, got, ok, want, wantOK)
			}
		}
	}
	for _, order := range [][]string{{"GET", "PUT", "POST"}, {"PUT", "POST", "GET"}} {
		registrar := &moduleRegistrar{mux: http.NewServeMux()}
		for _, method := range order {
			registrar.HandleFunc(method+" /s02/{id}", func(http.ResponseWriter, *http.Request) {})
		}
		if methods, _ := moduleRouteMethods(registrar.rules, "/s02/a"); methods.allow() != "GET, POST, PUT" {
			t.Fatal("method ordering depends on registration order", methods)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			for _, path := range s02PathCorpus() {
				path = httptest.NewRequest("GET", path, nil).URL.Path
				methods, ok := s.routeMethods(path)
				want, wantOK := s02BaselineRouteMethod(path)
				if methods.allow() != strings.ReplaceAll(want, "_OR_", ", ") || ok != wantOK {
					t.Errorf("concurrent query changed rules for %s", path)
				}
			}
		})
	}
	wg.Wait()
}

type s02UnreachablePlans struct{ ConfigPlans }
type s02UnreachableOperations struct{ useroperationshttp.Operations }

func TestModuleMethodRejectedWrites(t *testing.T) {
	reader := &constructionReader{}
	// Embedded capabilities panic if unexpectedly called, including writes.
	s := newTestServer(t, testHTTPConfig(true), nil, Modules{Nodes: reader, ConfigPlans: &s02UnreachablePlans{}, UserOperations: &s02UnreachableOperations{}}, Authorization{})
	for _, tc := range []struct{ method, path, allow string }{
		{"PUT", "/api/v1/nodes", "GET"},
		{"PUT", "/api/v1/nodes/" + baselineID, "GET"},
		{"POST", "/api/v1/nodes/" + baselineID + "/sessions", "GET"},
		{"POST", "/api/v1/nodes/" + baselineID + "/ip-bans", "GET"},
		{"POST", "/api/v1/nodes/" + baselineID + "/telemetry", "GET"},
		{"PUT", "/api/v1/nodes/" + baselineID + "/config-plans", "POST"},
		{"POST", "/api/v1/config-plans/" + baselineID, "GET"},
		{"PUT", "/api/v1/config-plans/" + baselineID + "/apply", "POST"},
		{"POST", "/api/v1/nodes/" + baselineID + "/users/alice/policy", "GET, PUT"},
		{"PUT", "/api/v1/user-batches", "POST"},
		{"POST", "/api/v1/user-batches/" + baselineID, "GET"},
		{"POST", "/api/v1/user-operations/metrics", "GET"},
	} {
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, baselineRequest(tc.method, tc.path, strings.NewReader("broken JSON")))
		if w.Code != 405 || w.Header().Get("Allow") != tc.allow || reader.calls != 0 {
			t.Fatalf("rejected write performed business work: %s %d calls=%d", tc.path, w.Code, reader.calls)
		}
	}
}
