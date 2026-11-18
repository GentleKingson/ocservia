package api

import (
	"context"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportevents"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/google/uuid"
)

func testHTTPConfig(devAuth bool) HTTPConfig {
	return HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1 << 20, RequestTimeout: 15 * time.Second, DevAuth: devAuth, BrowserOrigin: authTestOrigin, EventStreams: eventstream.DefaultConfig()}
}

type constructionFence struct{ ownersession.FencedExecutor }

type constructionReader struct {
	NodeReader
	calls int
}

func (r *constructionReader) ListNodesInWorkspace(context.Context, uuid.UUID, uuid.UUID, int) ([]telemetry.Node, bool, error) {
	r.calls++
	return []telemetry.Node{}, false, nil
}

func TestHTTPConstruction(t *testing.T) {
	config := testHTTPConfig(true)
	config.BrowserOrigin = "https://Admin.Example.Test"
	proxy := netip.MustParsePrefix("10.0.0.2/32")
	config.AuthTrustedProxies = []netip.Prefix{proxy}
	config.EventStreams.IdentityStreams, config.EventStreams.SessionStreams = 1, 1
	config.EventStreams.RetryAfter = 7 * time.Second
	wantEvents := config.EventStreams
	reader := &constructionReader{}
	plans := configplan.NewBackend(nil, nil)
	authz := &auth.Service{}
	modules := Modules{Nodes: reader, ConfigPlans: plans,
		Operations: operations.NewBackend(nil, 5, nil), TransportEvents: transportevents.NewBackend(nil, nil),
		Certificates: &certificates.Service{}, ReleaseCatalog: &releasecatalog.Catalog{},
		UserState:  userstate.NewWithSignerBackend(nil, nil),
		Enrollment: enrollment.NewBackend(nil, "", "test", nil), Transport: &transportclient.Client{}, OwnerFencing: &constructionFence{},
	}
	wantModules := modules
	authorization := Authorization{Authentication: authz}
	s := newTestServer(t, config, nil, modules, authorization)
	other := newTestServer(t, config, nil, modules, authorization)
	config.AuthTrustedProxies[0] = netip.MustParsePrefix("10.0.0.3/32")
	config.EventStreams = eventstream.Config{}
	config.BrowserOrigin, config.DevAuth = "https://changed.example", false
	modules, authorization = Modules{}, Authorization{}
	if s.authProxies[0] != proxy || s.browserOrigin != "https://admin.example.test" || !s.devAuth || s.auth != authz || s.configPlans != plans || s.nodes != reader {
		t.Fatal("constructor retained mutable input or lost shared service")
	}
	if s.operations != wantModules.Operations || s.transportEvents != wantModules.TransportEvents ||
		s.certificates != wantModules.Certificates || s.releaseCatalog != wantModules.ReleaseCatalog ||
		s.userstate != wantModules.UserState ||
		s.enrollment != wantModules.Enrollment || s.transport != wantModules.Transport || s.fences != wantModules.OwnerFencing {
		t.Fatal("constructor lost a configured capability")
	}
	w := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(w, baselineRequest("GET", "/api/v1/nodes", nil))
	if w.Code != 200 || reader.calls != 1 {
		t.Fatalf("first read: %d calls=%d", w.Code, reader.calls)
	}
	if s.eventAdmission == other.eventAdmission || s.platformEvents == other.platformEvents || s.operationEvents == other.operationEvents || s.breakGlassBudget == other.breakGlassBudget || s.localLoginBudget == other.localLoginBudget {
		t.Fatal("HTTP-owned state shared across Servers")
	}
	for i := 0; i < 3; i++ {
		for _, operation := range []bool{false, true} {
			got, admission, hub := s.eventStreamComponents(operation)
			wantHub := s.platformEvents
			if operation {
				wantHub = s.operationEvents
			}
			if got != wantEvents || admission != s.eventAdmission || hub != wantHub {
				t.Fatal("component access changed identity or config")
			}
			if snapshot := hub.Snapshot(); snapshot.Watchers != 0 || snapshot.Queries != 0 {
				t.Fatal("construction started polling", snapshot)
			}
		}
	}
	key := eventstream.AdmissionKey{Identity: "same", Session: "same", Workspace: "same", Resource: "same"}
	lease, err := s.eventAdmission.Acquire(key)
	testutil.NoError(t, err)
	defer lease.Release()
	if _, err := s.eventAdmission.Acquire(key); !errors.Is(err, eventstream.ErrIdentityLimit) {
		t.Fatal("final limit not applied", err)
	}
	independent, err := other.eventAdmission.Acquire(key)
	if err != nil {
		t.Fatal("cross-Server admission pollution", err)
	}
	independent.Release()
	admission, platform, operations := s.eventAdmission, s.platformEvents, s.operationEvents
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []bool{false, true} {
		_, gotAdmission, hub := s.eventStreamComponents(operation)
		if gotAdmission != admission || s.platformEvents != platform || s.operationEvents != operations {
			t.Fatal("shutdown replaced components")
		}
		if _, err := hub.Subscribe(t.Context(), "late", uuid.Nil); !errors.Is(err, eventstream.ErrClosed) {
			t.Fatal("late subscription", err)
		}
	}
}

func TestHTTPConstructionTypedNil(t *testing.T) {
	var nodes *telemetry.Service
	var plans *configplan.Service
	var operations *useroperations.Service
	s := newTestServer(t, testHTTPConfig(true), nil, Modules{Nodes: nodes, ConfigPlans: plans, UserOperations: operations}, Authorization{})
	if s.nodes != nil || s.configPlans != nil || s.userOperations != nil {
		t.Fatal("typed nil retained")
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/nodes"}, {"GET", "/api/v1/config-plans/" + baselineID},
		{"POST", "/api/v1/nodes/" + baselineID + "/config-plans"}, {"GET", "/api/v1/nodes/" + baselineID + "/users/alice/policy"},
	} {
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, baselineRequest(route.method, route.path, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: %d %s", route.path, w.Code, w.Body)
		}
	}
}

func TestHTTPConstructionSimulatorIsExplicit(t *testing.T) {
	for _, events := range []bool{false, true} {
		for _, simulator := range []bool{false, true} {
			for _, authenticated := range []bool{false, true} {
				config := testHTTPConfig(authenticated)
				config.LocalSimulator = simulator
				modules := Modules{}
				if events {
					modules.TransportEvents = transportevents.NewBackend(nil, nil)
				}
				s := newTestServer(t, config, nil, modules, Authorization{})
				// Final configuration is copied; an event source must not enable simulation.
				config.LocalSimulator = !simulator
				w := httptest.NewRecorder()
				r := baselineRequest("POST", "/api/v1/development/simulations", strings.NewReader("null"))
				r.Header.Set("Content-Type", "application/json")
				s.http.Handler.ServeHTTP(w, r)
				want := http.StatusNotFound
				if events && simulator {
					want = http.StatusUnauthorized
					if authenticated {
						want = http.StatusBadRequest
					}
				}
				if w.Code != want {
					t.Fatalf("events=%t simulator=%t authenticated=%t: %d want %d: %s", events, simulator, authenticated, w.Code, want, w.Body)
				}
				// Event routes remain registered and enforce authentication independently.
				w = httptest.NewRecorder()
				s.http.Handler.ServeHTTP(w, baselineRequest("GET", "/api/v1/events?after=invalid", nil))
				want = http.StatusUnauthorized
				if authenticated {
					want = http.StatusNotFound
					if events {
						want = http.StatusBadRequest
					}
				}
				if w.Code != want {
					t.Fatalf("event route events=%t simulator=%t authenticated=%t: %d want %d: %s", events, simulator, authenticated, w.Code, want, w.Body)
				}
			}
		}
	}
}

func TestHTTPConstructionRejectsInvalidSSE(t *testing.T) {
	for _, invalid := range []eventstream.Config{{}, {GlobalStreams: 1}, func() eventstream.Config {
		c := eventstream.DefaultConfig()
		c.Watchers = c.GlobalStreams + 1
		return c
	}()} {
		config := testHTTPConfig(false)
		config.EventStreams = invalid
		s, err := NewServer(config, nil, BuildInfo{}, slog.Default(), Modules{}, Authorization{})
		if s != nil || err == nil || !strings.Contains(err.Error(), "configure SSE admission: event stream configuration is outside the safe range") {
			t.Fatalf("invalid SSE returned a runnable Server or lost diagnostics: %v %v", s, err)
		}
	}
}

func TestHTTPAssemblyBoundary(t *testing.T) {
	server := reflect.TypeOf(Server{})
	for i := 0; i < server.NumField(); i++ {
		field := server.Field(i)
		if field.Type == reflect.TypeOf(Modules{}) || field.Type == reflect.TypeOf(Authorization{}) || field.Type == reflect.TypeOf(HTTPConfig{}) {
			t.Errorf("Server stores assembly input: %s", field.Name)
		}
	}
	for _, name := range []string{"EnableTelemetry", "EnableConfigPlans", "EnableUserOperations", "EnableAuthorization", "EnableCertificates", "ConfigureEventStreams", "EnableOperations", "EnableReleaseCatalog", "EnableUserState", "EnablePrivdAttestation", "EnableEnrollment", "EnableOwnerFencing", "EnableTransportEvents", "EnableBrowserOrigin", "ConfigureAuthProxies", "SetLocalSimulatorEnabled"} {
		if _, ok := reflect.TypeOf(&Server{}).MethodByName(name); ok {
			t.Errorf("mutable assembly remains: %s", name)
		}
	}
	// Count production constructor sites as well as their owner. Runtime zero
	// watcher counts alone cannot prove that a discarded default set never existed.
	counts := map[string]int{}
	files, err := filepath.Glob("*.go")
	testutil.NoError(t, err)
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		testutil.NoError(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Recv == nil && fn.Name.Name == "NewBackend" {
				t.Error("compatibility constructor remains")
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "NewManager", "NewWatcherBudget", "NewHubWithWatcherBudget", "NewHub":
					counts[sel.Sel.Name]++
					if fn.Name.Name != "initEventStreams" {
						t.Errorf("SSE constructed in %s", fn.Name.Name)
					}
				case "initEventStreams":
					counts[sel.Sel.Name]++
					if fn.Name.Name != "NewServer" {
						t.Errorf("SSE initialization outside constructor: %s", fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if !reflect.DeepEqual(counts, map[string]int{"NewManager": 1, "NewWatcherBudget": 1, "NewHubWithWatcherBudget": 2, "initEventStreams": 1}) {
		t.Fatal("SSE construction sites", counts)
	}
}

func newTestServer(t *testing.T, config HTTPConfig, backend database.Backend, modules Modules, authorization Authorization) *Server {
	t.Helper()
	s, err := NewServer(config, backend, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), modules, authorization)
	testutil.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}
