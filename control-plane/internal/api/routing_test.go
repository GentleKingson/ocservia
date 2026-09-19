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
	t.Run("ordinary-static", func(t *testing.T) {
		mux := http.NewServeMux()
		registrar := &methodRegistrar{mux: mux}
		calls := 0
		registrar.HandleFunc("POST /s03/ordinary", func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(202)
		})
		s := &Server{registeredMethods: registrar.rules}
		if _, ok := s02BaselineRouteMethod("/s03/ordinary"); ok {
			t.Fatal("test route exists in the old method table")
		}
		for _, method := range []string{"POST", "GET", "HEAD", "OPTIONS", "BREW"} {
			w := httptest.NewRecorder()
			s.routeErrors(mux).ServeHTTP(w, httptest.NewRequest(method, "/s03/ordinary", nil))
			if method == "POST" {
				if w.Code != 202 || calls != 1 {
					t.Fatal("ordinary handler not dispatched exactly once")
				}
			} else if w.Code != 405 || w.Header().Get("Allow") != "POST" || calls != 1 {
				t.Fatalf("ordinary method denial: %d %v calls=%d", w.Code, w.Header(), calls)
			}
		}
	})
	mux := http.NewServeMux()
	registrar := &methodRegistrar{mux: mux}
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
	s.registeredMethods = registrar.rules
	if _, ok := compatibilityRouteMethod("/s02/a"); ok {
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
	s.registeredMethods = registrar.rules
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
			registrar := &methodRegistrar{mux: http.NewServeMux()}
			mustRegistrationPanic(t, func() { registrar.HandleFunc(pattern, handler) })
			if len(registrar.rules) != 0 {
				t.Fatal("failed construction published metadata", registrar.rules)
			}
		})
	}
	for _, pattern := range []string{"GET /s02/{id}", "POST /s02/fixed", "POST /s02/{other}"} {
		t.Run("conflict-"+pattern, func(t *testing.T) {
			mux := http.NewServeMux()
			registrar := &methodRegistrar{mux: mux}
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
		registrar := &methodRegistrar{mux: mux}
		mustRegistrationPanic(t, func() { registrar.HandleFunc("GET /s02/{id}", handler) })
		if len(registrar.rules) != 0 {
			t.Fatal("root mux failure published metadata")
		}
	})
	t.Run("nil-handler", func(t *testing.T) {
		mux := http.NewServeMux()
		registrar := &methodRegistrar{mux: mux}
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
	// Counterfactual methods make an erroneous fallback observable: the detail
	// compatibility rule would allow GET, but a derived hit must end lookup.
	for _, path := range []string{"/api/v1/operations/summary", "/api/v1/operations/queue-metrics"} {
		mux := http.NewServeMux()
		registrar := &methodRegistrar{mux: mux}
		calls := 0
		registrar.HandleFunc("PUT "+path, func(http.ResponseWriter, *http.Request) { calls++ })
		s := &Server{registeredMethods: registrar.rules}
		if method, ok := compatibilityRouteMethod(path); !ok || method != "GET" {
			t.Fatal("counterfactual must overlap the operation detail rule")
		}
		w := httptest.NewRecorder()
		s.routeErrors(mux).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 405 || w.Header().Get("Allow") != "PUT" || calls != 0 {
			t.Fatalf("derived rejection fell back to compatibility: %d %v calls=%d", w.Code, w.Header(), calls)
		}
	}
	production := baselineServer(t, false)
	for _, path := range []string{"/api/v1/operations/summary", "/api/v1/operations/queue-metrics"} {
		if methods, ok := registeredRouteMethods(production.registeredMethods, path); !ok || methods.allow() != "GET" {
			t.Fatal("production static route not derived", methods, ok)
		}
	}
	for _, line := range strings.Split(compatibilityBaseline, "\n") {
		_, path := baselineRoutePath(line, "")
		if line == "POST /api/v1/approval-requests/{approval_id}" {
			path += ":approve"
		}
		want, wantOK := s02BaselineRouteMethod(path)
		if methods, ok := production.routeMethods(path); !ok || !wantOK || methods.allow() != want {
			t.Fatalf("compatibility route changed: %s %v/%v", path, methods, ok)
		}
	}
}

func TestModuleMethodIsolationAndOrder(t *testing.T) {
	s, other := baselineServer(t, false), baselineServer(t, false)
	if &s.registeredMethods[0] == &other.registeredMethods[0] || &s.registeredMethods[0].methods[0] == &other.registeredMethods[0].methods[0] {
		t.Fatal("Servers share method metadata")
	}
	// Reorder all actual registration groups, not a duplicate test route table.
	registrations := []func(*methodRegistrar){
		func(r *methodRegistrar) { s.registerHealthRoutes(r) },
		func(r *methodRegistrar) { s.registerAuthRoutes(r, r.mux) },
		func(r *methodRegistrar) { s.registerDevelopmentRoutes(r) },
		func(r *methodRegistrar) { s.registerOperationsRoutes(r, r.mux) },
		func(r *methodRegistrar) { s.registerEnrollmentRoutes(r) },
		func(r *methodRegistrar) { s.registerNodeRoutes(r) },
		func(r *methodRegistrar) { s.registerUserRoutes(r, r.mux) },
		func(r *methodRegistrar) { s.registerConfigPlanRoutes(r) },
		func(r *methodRegistrar) { s.registerCertificateRoutes(r, r.mux) },
		func(r *methodRegistrar) { s.registerAuthorizationRoutes(r, r.mux) },
	}
	for _, order := range [][]int{{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, {9, 8, 7, 6, 5, 4, 3, 2, 1, 0}, {4, 5, 6, 7, 8, 9, 0, 1, 2, 3}} {
		registrar := &methodRegistrar{mux: http.NewServeMux()}
		for _, i := range order {
			registrations[i](registrar)
		}
		for _, path := range ordinaryPathCorpus() {
			path = httptest.NewRequest("GET", path, nil).URL.Path
			want, wantOK := registeredRouteMethods(s.registeredMethods, path)
			got, ok := registeredRouteMethods(registrar.rules, path)
			if !slices.Equal(got, want) || ok != wantOK {
				t.Fatalf("order=%v path=%s: %v/%v != %v/%v", order, path, got, ok, want, wantOK)
			}
		}
	}
	for _, order := range [][]string{{"GET", "PUT", "POST"}, {"PUT", "POST", "GET"}} {
		registrar := &methodRegistrar{mux: http.NewServeMux()}
		for _, method := range order {
			registrar.HandleFunc(method+" /s02/{id}", func(http.ResponseWriter, *http.Request) {})
		}
		if methods, _ := registeredRouteMethods(registrar.rules, "/s02/a"); methods.allow() != "GET, POST, PUT" {
			t.Fatal("method ordering depends on registration order", methods)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			for _, path := range ordinaryPathCorpus() {
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
	for _, line := range strings.Split(routeBaseline, "\n") {
		fields := strings.Split(line, "|")
		_, path := baselineRoutePath(fields[0], fields[1])
		for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
			if slices.Contains(strings.Split(fields[2], ", "), method) {
				continue
			}
			w := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, baselineRequest(method, path, strings.NewReader("broken JSON")))
			if w.Code != 405 || w.Header().Get("Allow") != fields[2] || reader.calls != 0 {
				t.Fatalf("rejected write escaped method check: %s %s %d %v", method, path, w.Code, w.Header())
			}
		}
	}
}
