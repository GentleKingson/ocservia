package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/google/uuid"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func bareServer(t *testing.T, devAuth bool, devToken string) *Server {
	t.Helper()
	server, err := NewServer(HTTPConfig{
		Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second,
		DevAuth: devAuth, DevAuthToken: devToken, EventStreams: eventstream.DefaultConfig(),
	}, nil, BuildInfo{Version: "test", Commit: "abc"}, quietLogger(), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestProblemErrorClassification(t *testing.T) {
	server := &Server{logger: quietLogger()}
	cases := []struct {
		name        string
		write       func(http.ResponseWriter, *http.Request)
		wantStatus  int
		wantProblem string
	}{
		{"rollout not-found", func(w http.ResponseWriter, r *http.Request) {
			server.writeRolloutError(w, r, errors.Join(errors.New("ctx"), database.ErrNotFound))
		}, http.StatusNotFound, "https://ocservia.dev/problems/not-found"},
		{"operation not-found", func(w http.ResponseWriter, r *http.Request) {
			server.writeOperationError(w, r, errors.Join(errors.New("ctx"), database.ErrNotFound))
		}, http.StatusNotFound, "https://ocservia.dev/problems/not-found"},
		{"certificate not-found", func(w http.ResponseWriter, r *http.Request) {
			writeCertificateError(w, r, errors.Join(errors.New("ctx"), database.ErrNotFound))
		}, http.StatusNotFound, "https://ocservia.dev/problems/not-found"},
		{"userstate capacity", func(w http.ResponseWriter, r *http.Request) {
			server.writeUserStateError(w, r, userstate.ErrCapacityExceeded)
		}, http.StatusConflict, "https://ocservia.dev/problems/capacity-exceeded"},
		{"userstate pending", func(w http.ResponseWriter, r *http.Request) {
			server.writeUserStateError(w, r, userstate.ErrRevisionPending)
		}, http.StatusConflict, "https://ocservia.dev/problems/desired-revision-pending"},
		{"userstate recovery", func(w http.ResponseWriter, r *http.Request) {
			server.writeUserStateError(w, r, userstate.ErrRevisionRecovery)
		}, http.StatusConflict, "https://ocservia.dev/problems/desired-revision-recovery-required"},
		{"operation backlog", func(w http.ResponseWriter, r *http.Request) {
			server.writeOperationError(w, r, operations.ErrBacklogExceeded)
		}, http.StatusServiceUnavailable, "https://ocservia.dev/problems/command-backlog-exceeded"},
		{"enrollment invalid", func(w http.ResponseWriter, r *http.Request) {
			server.writeEnrollmentError(w, r, enrollment.ErrInvalidRequest)
		}, http.StatusBadRequest, ""},
		{"enrollment unavailable", func(w http.ResponseWriter, r *http.Request) {
			server.writeEnrollmentError(w, r, errors.New("database unavailable"))
		}, http.StatusServiceUnavailable, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.write(rec, httptest.NewRequest(http.MethodPost, "/api/v1/test", nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d want %d body = %s", rec.Code, tc.wantStatus, rec.Body)
			}
			if tc.wantProblem != "" && !strings.Contains(rec.Body.String(), tc.wantProblem) {
				t.Fatalf("body missing %q: %s", tc.wantProblem, rec.Body)
			}
		})
	}
}

func TestLiveVersionRoutingAndCacheControl(t *testing.T) {
	server := bareServer(t, false, "")
	rec := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("X-Request-ID") == "" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("livez: status=%d rid=%q ct=%q", rec.Code, rec.Header().Get("X-Request-ID"), rec.Header().Get("Content-Type"))
	}

	rec = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/version", nil))
	var version map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &version); err != nil || version["version"] != "test" || version["commit"] != "abc" || version["role"] != "all" {
		t.Fatalf("version: err=%v body=%v", err, version)
	}

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/api/v1/livez", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/auth/login", http.StatusMethodNotAllowed},
		{http.MethodGet, "/missing", http.StatusNotFound},
		{http.MethodGet, "/api/v1/development/runtime", http.StatusNotFound},
	} {
		rec = httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want || rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Fatalf("%s %s: status=%d ct=%q", tc.method, tc.path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}

	for _, tc := range []struct{ path, cache string }{
		{"/api/v1/version", "private, max-age=60"}, {"/version", "private, max-age=60"},
		{"/api/v1/nodes", "no-store"}, {"/api/v1/not-a-route", "no-store"}, {"/api/v1/auth/session", "no-store"}, {"/healthz", ""},
	} {
		rec = httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if got := rec.Header().Get("Cache-Control"); got != tc.cache {
			t.Fatalf("%s Cache-Control=%q want %q", tc.path, got, tc.cache)
		}
	}
}

func TestProtectedRoutesRequireAuthentication(t *testing.T) {
	server := bareServer(t, false, "")
	const node = "019fc0a4-6d92-765c-a8a1-4af556614cc3"
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/operations"},
		{http.MethodGet, "/api/v1/operations/" + node},
		{http.MethodPost, "/api/v1/node-bootstrap-tokens"},
		{http.MethodPost, "/api/v1/nodes/" + node + "/revocation"},
		{http.MethodPost, "/api/v1/nodes/" + node + "/synthetic-commands"},
		{http.MethodPost, "/api/v1/nodes/" + node + "/sessions/42:disconnect"},
		{http.MethodPost, "/api/v1/nodes/" + node + "/sessions/42:terminate"},
		{http.MethodPost, "/api/v1/nodes/" + node + "/ip-bans/192.0.2.9:remove"},
		{http.MethodPost, "/api/v1/nodes/" + node + "/service:reload"},
		{http.MethodPost, "/api/v1/nodes/" + node + "/agent-upgrade"},
		{http.MethodGet, "/api/v1/nodes/" + node + "/users/alice/policy"},
		{http.MethodPut, "/api/v1/nodes/" + node + "/users/alice/policy"},
	}
	for _, r := range routes {
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, httptest.NewRequest(r.method, r.path, strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want 401", r.method, r.path, rec.Code)
		}
	}
}

func TestDevAuthAndReboundHostProtection(t *testing.T) {
	dev := bareServer(t, true, "")
	for host, wantUnauth := range map[string]bool{"attacker.example:8080": true, "127.0.0.1:8080": false} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/operations", nil)
		req.Host = host
		dev.http.Handler.ServeHTTP(rec, req)
		if (rec.Code == http.StatusUnauthorized) != wantUnauth || (wantUnauth && rec.Header().Get("X-Ocservia-Dev-Subject") != "") {
			t.Fatalf("devAuth host=%s status=%d", host, rec.Code)
		}
	}

	const token = "local-development-token-32-characters"
	bearer := bareServer(t, false, token)
	for _, tc := range []struct {
		host, auth string
		wantUnauth bool
	}{
		{"127.0.0.1:8080", "wrong", true},
		{"attacker.example:8080", token, true},
		{"127.0.0.1:8080", token, false},
		{"localhost:8080", token, false},
		{"[::1]:8080", token, false},
		{"web:4173", token, false},
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
	cookiePrincipal := auth.Principal{IdentityID: uuid.Must(uuid.NewV7()), SessionID: uuid.Must(uuid.NewV7()), Issuer: "https://id.example.test"}
	cases := []struct {
		name, trustedOrigin, method, origin, fetchSite string
		principal                                      auth.Principal
		allowed                                        bool
	}{
		{"same origin", "", http.MethodPost, "https://admin.example.com", "", cookiePrincipal, true},
		{"default https port", "https://admin.example.com:443", http.MethodPost, "https://admin.example.com", "", cookiePrincipal, true},
		{"same-origin fetch", "", http.MethodPost, "https://admin.example.com", "same-origin", cookiePrincipal, true},
		{"sibling origin rejected", "", http.MethodPost, "https://app.example.com", "", cookiePrincipal, false},
		{"missing origin fails closed", "", http.MethodPost, "", "", cookiePrincipal, false},
		{"port mismatch rejected", "", http.MethodPost, "https://admin.example.com:8443", "", cookiePrincipal, false},
		{"scheme mismatch rejected", "", http.MethodPost, "http://admin.example.com", "", cookiePrincipal, false},
		{"cross-site fetch rejected", "", http.MethodPost, "https://admin.example.com", "cross-site", cookiePrincipal, false},
		{"safe method skips origin", "", http.MethodGet, "", "", cookiePrincipal, true},
		{"dev principal skips boundary", "", http.MethodPost, "", "", auth.Principal{Subject: "developer", Issuer: "development", BreakGlass: true}, true},
		{"break-glass requires exact origin", "", http.MethodPost, "https://admin.example.com", "", auth.Principal{Subject: "offline", Issuer: "break-glass", BreakGlass: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testHTTPConfig(false)
			cfg.BrowserOrigin = tc.trustedOrigin
			if cfg.BrowserOrigin == "" {
				cfg.BrowserOrigin = "https://admin.example.com"
			}
			server := newTestServer(t, cfg, nil, Modules{}, Authorization{})
			req := httptest.NewRequest(tc.method, "/api/v1/node-bootstrap-tokens", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			err := server.validateBrowserMutation(req, tc.principal)
			if tc.allowed && err != nil {
				t.Fatalf("expected allowed, got %v", err)
			}
			if !tc.allowed && !errors.Is(err, errCrossOrigin) {
				t.Fatalf("expected errCrossOrigin, got %v", err)
			}
		})
	}

	// Unconfigured browser origin fails closed; break-glass rejects cross-site.
	unconfigured := &Server{logger: quietLogger()}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/node-bootstrap-tokens", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	if err := unconfigured.validateBrowserMutation(req, auth.Principal{Issuer: "local"}); !errors.Is(err, errCrossOrigin) {
		t.Fatalf("unconfigured origin = %v, want errCrossOrigin", err)
	}

	cfg := testHTTPConfig(false)
	cfg.BrowserOrigin = "https://admin.example.com"
	bgServer := newTestServer(t, cfg, nil, Modules{}, Authorization{Authentication: &auth.Service{}})
	for _, tc := range []struct{ origin, fetchSite string }{
		{"https://app.example.com", ""}, {"https://admin.example.com", "cross-site"}, {"", ""},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/break-glass", strings.NewReader(`{"token":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if tc.fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
		}
		rec := httptest.NewRecorder()
		bgServer.breakGlass(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "https://ocservia.dev/problems/cross-origin-request") {
			t.Fatalf("break-glass (%q,%q) status=%d body=%s", tc.origin, tc.fetchSite, rec.Code, rec.Body)
		}
	}
}

func TestJSONMediaTypeGateAndForeignPayloadFields(t *testing.T) {
	server := &Server{logger: quietLogger(), devAuth: true, enrollment: &enrollment.Service{}, operations: operations.NewBackend(nil, 1, nil)}
	for _, tc := range []struct {
		ct, body string
		status   int
	}{
		{"text/plain", `{"reason":"test"}`, http.StatusUnsupportedMediaType},
		{"application/x-www-form-urlencoded", "reason=test", http.StatusUnsupportedMediaType},
		{"", `{"reason":"test"}`, http.StatusUnsupportedMediaType},
		{"application/json", `{"reason":"test"}`, http.StatusBadRequest},
		{"application/json; charset=utf-8", "{", http.StatusBadRequest},
		{"application/json", `{"reason":"test","extra":1}`, http.StatusBadRequest},
		{"application/json", `{"reason":"test"}{"reason":"second"}`, http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/node-bootstrap-tokens", strings.NewReader(tc.body))
		if tc.ct != "" {
			req.Header.Set("Content-Type", tc.ct)
		}
		rec := httptest.NewRecorder()
		server.createNodeBootstrapToken(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("ct=%q body=%q status=%d want %d", tc.ct, tc.body, rec.Code, tc.status)
		}
	}

	for _, tc := range []struct {
		body    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{`{"kind":"noop","message":"unused","expected_version":1}`, server.createSyntheticCommand},
		{`{"kind":"service_reload","expected_version":1}`, server.createSyntheticCommand},
		{`{"boot_id":"unused","expected_version":1,"reason":"support"}`, server.reloadService},
		{`{"boot_id":"unused","expected_version":1,"reason":"support"}`, func(w http.ResponseWriter, r *http.Request) {
			server.createControlledCommand(w, r, operations.IPBanRemove, "ip_ban.remove", "", "192.0.2.1")
		}},
	} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
		req.Header.Set("Idempotency-Key", "payload-boundary")
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("node_id", "01900000-0000-7000-8000-000000000001")
		rec := httptest.NewRecorder()
		tc.handler(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid-request") {
			t.Fatalf("body=%s status=%d resp=%s", tc.body, rec.Code, rec.Body)
		}
	}
}

func TestExpectedRevisionAndEnrollmentTTLValidation(t *testing.T) {
	for _, s := range []int64{-1, 901, 36028797018963968} {
		if validEnrollmentTTLSeconds(s) {
			t.Fatalf("ttl %d accepted", s)
		}
	}
	for _, s := range []int64{0, 1, 900} {
		if !validEnrollmentTTLSeconds(s) {
			t.Fatalf("ttl %d rejected", s)
		}
	}
	v := int64(7)
	for _, tc := range []struct {
		header string
		body   *int64
		want   int64
		ok     bool
	}{
		{`"revision-7"`, nil, 7, true},
		{"", &v, 7, true},
		{`"revision-7"`, &v, 7, true},
		{"revision-6", &v, 0, false},
		{"*", nil, 0, false},
	} {
		if got, ok := expectedRevision(tc.header, tc.body); got != tc.want || ok != tc.ok {
			t.Fatalf("expectedRevision(%q) = (%d,%v) want (%d,%v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}
