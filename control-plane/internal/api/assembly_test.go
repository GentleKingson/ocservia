package api

import (
	"context"
	"errors"
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

	"github.com/GentleKingson/ocservia/control-plane/internal/api/configplanhttp"
	"github.com/GentleKingson/ocservia/control-plane/internal/api/nodehttp"
	"github.com/GentleKingson/ocservia/control-plane/internal/api/useroperationshttp"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/google/uuid"
)

func testHTTPConfig(devAuth bool) HTTPConfig {
	return HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1 << 20, RequestTimeout: 15 * time.Second, DevAuth: devAuth, BrowserOrigin: authTestOrigin, EventStreams: eventstream.DefaultConfig()}
}

type constructionReader struct {
	nodehttp.Reader
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
	authz := rbac.NewBackend(nil)
	modules, authorization := Modules{Nodes: reader, ConfigPlans: plans}, Authorization{RBAC: authz}
	s := newTestServer(t, config, nil, modules, authorization)
	other := newTestServer(t, config, nil, modules, authorization)
	config.AuthTrustedProxies[0] = netip.MustParsePrefix("10.0.0.3/32")
	config.EventStreams = eventstream.Config{}
	config.BrowserOrigin, config.DevAuth = "https://changed.example", false
	modules.Nodes, modules.ConfigPlans, authorization.RBAC = nil, nil, nil
	if s.authProxies[0] != proxy || s.browserOrigin != "https://admin.example.test" || !s.devAuth || s.rbac != authz || s.configPlanLookup != plans {
		t.Fatal("constructor retained mutable input or lost shared service")
	}
	w := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(w, baselineRequest("GET", "/api/v1/nodes", nil))
	if w.Code != 200 || reader.calls != 1 {
		t.Fatalf("first read: %d calls=%d", w.Code, reader.calls)
	}
	if s.nodeHTTP == other.nodeHTTP || s.configPlanHTTP == other.configPlanHTTP || s.userOpsHTTP == other.userOpsHTTP || s.eventAdmission == other.eventAdmission || s.platformEvents == other.platformEvents || s.operationEvents == other.operationEvents || s.breakGlassBudget == other.breakGlassBudget || s.localLoginBudget == other.localLoginBudget {
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
	if err != nil {
		t.Fatal(err)
	}
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
	var roles *rbac.Service
	s := newTestServer(t, testHTTPConfig(true), nil, Modules{Nodes: nodes, ConfigPlans: plans, UserOperations: operations}, Authorization{RBAC: roles})
	if s.configPlanLookup != nil || s.rbac != nil {
		t.Fatal("typed nil retained")
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/nodes"}, {"GET", "/api/v1/config-plans/" + baselineID},
		{"POST", "/api/v1/nodes/" + baselineID + "/config-plans"}, {"GET", "/api/v1/user-operations/metrics"},
	} {
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, baselineRequest(route.method, route.path, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: %d %s", route.path, w.Code, w.Body)
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
	for _, handler := range []reflect.Type{reflect.TypeOf(&nodehttp.Handler{}), reflect.TypeOf(&configplanhttp.Handler{}), reflect.TypeOf(&useroperationshttp.Handler{})} {
		if handler.NumMethod() != 1 || handler.Method(0).Name != "Register" {
			t.Errorf("module exposes mutable capabilities: %s", handler)
		}
	}
	server := reflect.TypeOf(Server{})
	for i := 0; i < server.NumField(); i++ {
		field := server.Field(i)
		if field.Type == reflect.TypeOf(Modules{}) || field.Type == reflect.TypeOf(Authorization{}) || field.Type == reflect.TypeOf(HTTPConfig{}) {
			t.Errorf("Server stores assembly input: %s", field.Name)
		}
	}
	for _, name := range []string{"EnableTelemetry", "EnableConfigPlans", "EnableUserOperations", "EnableAuthorization", "EnableCertificates", "ConfigureEventStreams"} {
		if _, ok := reflect.TypeOf(&Server{}).MethodByName(name); ok {
			t.Errorf("mutable assembly remains: %s", name)
		}
	}
	// Count production constructor sites as well as their owner. Runtime zero
	// watcher counts alone cannot prove that a discarded default set never existed.
	counts := map[string]int{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
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
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}
