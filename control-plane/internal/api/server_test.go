package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/google/uuid"
)

const authTestOrigin = "https://admin.example.test"

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testHTTPConfig(devAuth bool) HTTPConfig {
	return HTTPConfig{
		Address: "127.0.0.1:0", BodyLimit: 1 << 20, RequestTimeout: 15 * time.Second,
		DevAuth: devAuth, BrowserOrigin: authTestOrigin, EventStreams: eventstream.DefaultConfig(),
	}
}

func newTestServer(t *testing.T, config HTTPConfig, backend database.Backend, modules Modules, authorization Authorization) *Server {
	t.Helper()
	s, err := NewServer(config, backend, BuildInfo{Version: "test", Commit: "abc"}, quietLogger(), modules, authorization)
	testutil.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s
}

func TestHTTPConstructionTypedNil(t *testing.T) {
	t.Parallel()
	var nodes *telemetry.Service
	var plans *configplan.Service
	var ops *useroperations.Service
	s := newTestServer(t, testHTTPConfig(true), nil, Modules{Nodes: nodes, ConfigPlans: plans, UserOperations: ops}, Authorization{})
	if s.nodes != nil || s.configPlans != nil || s.userOperations != nil {
		t.Fatal("typed nil retained")
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/nodes"},
		{"GET", "/api/v1/config-plans/" + baselineID},
		{"POST", "/api/v1/nodes/" + baselineID + "/config-plans"},
		{"GET", "/api/v1/nodes/" + baselineID + "/users/alice/policy"},
	} {
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, baselineRequest(route.method, route.path, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: %d %s", route.path, w.Code, w.Body)
		}
	}
}

func TestProblemErrorClassification(t *testing.T) {
	t.Parallel()
	server := &Server{logger: quietLogger()}
	for _, tc := range []struct {
		name       string
		write      func(http.ResponseWriter, *http.Request)
		wantStatus int
	}{
		{"not-found", func(w http.ResponseWriter, r *http.Request) {
			server.writeOperationError(w, r, errors.Join(errors.New("ctx"), database.ErrNotFound))
		}, http.StatusNotFound},
		{"userstate pending", func(w http.ResponseWriter, r *http.Request) {
			server.writeUserStateError(w, r, userstate.ErrRevisionPending)
		}, http.StatusConflict},
		{"operation backlog", func(w http.ResponseWriter, r *http.Request) {
			server.writeOperationError(w, r, operations.ErrBacklogExceeded)
		}, http.StatusServiceUnavailable},
		{"enrollment invalid", func(w http.ResponseWriter, r *http.Request) {
			server.writeEnrollmentError(w, r, enrollment.ErrInvalidRequest)
		}, http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		tc.write(rec, httptest.NewRequest(http.MethodPost, "/api/v1/test", nil))
		if rec.Code != tc.wantStatus {
			t.Fatalf("%s: status = %d want %d", tc.name, rec.Code, tc.wantStatus)
		}
	}
}

func TestProtectedRoutesAndReboundHostProtection(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, testHTTPConfig(false), nil, Modules{}, Authorization{})
	const node = "019fc0a4-6d92-765c-a8a1-4af556614cc3"
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/operations"},
		{http.MethodPost, "/api/v1/node-bootstrap-tokens"},
		{http.MethodPost, "/api/v1/nodes/" + node + "/revocation"},
		{http.MethodPost, "/api/v1/nodes/" + node + "/service:reload"},
		{http.MethodPost, "/api/v1/nodes/" + node + "/agent-upgrade"},
		{http.MethodPut, "/api/v1/nodes/" + node + "/users/alice/policy"},
	} {
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, httptest.NewRequest(r.method, r.path, strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want 401", r.method, r.path, rec.Code)
		}
	}

	const token = "local-development-token-32-characters"
	cfg := testHTTPConfig(false)
	cfg.DevAuthToken = token
	bearer := newTestServer(t, cfg, nil, Modules{}, Authorization{})
	for _, tc := range []struct {
		host, auth string
		wantUnauth bool
	}{
		{"127.0.0.1:8080", "wrong", true},
		{"attacker.example:8080", token, true},
		{"127.0.0.1:8080", token, false},
		{"localhost:8080", token, false},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/operations", nil)
		req.Host = tc.host
		req.Header.Set("Authorization", "Bearer "+tc.auth)
		bearer.http.Handler.ServeHTTP(rec, req)
		if (rec.Code == http.StatusUnauthorized) != tc.wantUnauth {
			t.Fatalf("bearer host=%s status=%d", tc.host, rec.Code)
		}
	}
}

func TestBrowserMutationAndBreakGlassOriginBoundaries(t *testing.T) {
	t.Parallel()
	cookiePrincipal := auth.Principal{IdentityID: uuid.Must(uuid.NewV7()), SessionID: uuid.Must(uuid.NewV7()), Issuer: "https://id.example.test"}
	for _, tc := range []struct {
		name, method, origin, fetchSite string
		principal                       auth.Principal
		allowed                         bool
	}{
		{"same origin", http.MethodPost, "https://admin.example.com", "", cookiePrincipal, true},
		{"sibling origin rejected", http.MethodPost, "https://app.example.com", "", cookiePrincipal, false},
		{"missing origin fails closed", http.MethodPost, "", "", cookiePrincipal, false},
		{"port mismatch rejected", http.MethodPost, "https://admin.example.com:8443", "", cookiePrincipal, false},
		{"cross-site fetch rejected", http.MethodPost, "https://admin.example.com", "cross-site", cookiePrincipal, false},
		{"safe method skips origin", http.MethodGet, "", "", cookiePrincipal, true},
		{"break-glass requires exact origin", http.MethodPost, "https://admin.example.com", "", auth.Principal{Subject: "offline", Issuer: "break-glass", BreakGlass: true}, true},
	} {
		cfg := testHTTPConfig(false)
		cfg.BrowserOrigin = "https://admin.example.com"
		server := newTestServer(t, cfg, nil, Modules{}, Authorization{})
		req := httptest.NewRequest(tc.method, "/api/v1/node-bootstrap-tokens", nil)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if tc.fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
		}
		err := server.validateBrowserMutation(req, tc.principal)
		if tc.allowed != (err == nil) {
			t.Fatalf("%s: allowed=%v err=%v", tc.name, tc.allowed, err)
		}
	}

	cfg := testHTTPConfig(false)
	cfg.BrowserOrigin = "https://admin.example.com"
	bgServer := newTestServer(t, cfg, nil, Modules{}, Authorization{Authentication: &auth.Service{}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/break-glass", strings.NewReader(`{"token":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	bgServer.breakGlass(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("break-glass cross-origin status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestJSONMediaTypeGateAndForeignPayloadFields(t *testing.T) {
	t.Parallel()
	server := &Server{logger: quietLogger(), devAuth: true, enrollment: &enrollment.Service{}, operations: operations.New(nil, 1, nil)}
	for _, tc := range []struct {
		ct, body string
		status   int
	}{
		{"text/plain", `{"reason":"test"}`, http.StatusUnsupportedMediaType},
		{"application/json", `{"reason":"test","extra":1}`, http.StatusBadRequest},
		{"application/json", `{"reason":"test"}{"reason":"second"}`, http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/node-bootstrap-tokens", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", tc.ct)
		rec := httptest.NewRecorder()
		server.createNodeBootstrapToken(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("ct=%q status=%d want %d", tc.ct, rec.Code, tc.status)
		}
	}
}
