package api

import (
	"context"
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
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLiveAndRequestID(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{Version: "test"}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	request := httptest.NewRequest(http.MethodGet, "/livez", nil)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing request ID")
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
	}
}

func TestDevAuthMarker(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, true, "", 1)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if response.Header().Get("X-Ocservia-Dev-Subject") != "developer" {
		t.Fatal("development subject was not injected")
	}
}

func TestOperationsRequireAuthenticatedPrincipal(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	for _, path := range []string{"/api/v1/operations", "/api/v1/operations/019fc0a4-6d92-765c-a8a1-4af556614cc3"} {
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d", path, response.Code)
		}
		if response.Header().Get("WWW-Authenticate") != "OIDC" {
			t.Fatalf("%s missing OIDC challenge", path)
		}
	}
}

func TestUserStateCapacityErrorUsesConflictProblem(t *testing.T) {
	server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/node/users", nil)
	response := httptest.NewRecorder()
	server.writeUserStateError(response, request, userstate.ErrCapacityExceeded)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d", response.Code)
	}
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Type != "https://ocservia.dev/problems/capacity-exceeded" {
		t.Fatalf("problem type = %q", problem.Type)
	}
}

func TestOperationBacklogErrorUsesServiceUnavailableProblem(t *testing.T) {
	server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/node/synthetic-commands", nil)
	response := httptest.NewRecorder()
	server.writeOperationError(response, request, operationstore.ErrBacklogExceeded)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Type != "https://ocservia.dev/problems/command-backlog-exceeded" {
		t.Fatalf("problem type = %q", problem.Type)
	}
}

func TestUserStateRevisionSlotErrorsUseConflictProblems(t *testing.T) {
	tests := []struct {
		err         error
		problemType string
	}{
		{userstate.ErrRevisionPending, "https://ocservia.dev/problems/desired-revision-pending"},
		{userstate.ErrRevisionRecovery, "https://ocservia.dev/problems/desired-revision-recovery-required"},
	}
	server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, test := range tests {
		response := httptest.NewRecorder()
		server.writeUserStateError(response, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/node/users", nil), test.err)
		if response.Code != http.StatusConflict {
			t.Fatalf("%s status = %d", test.problemType, response.Code)
		}
		var problem struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
			t.Fatal(err)
		}
		if problem.Type != test.problemType {
			t.Fatalf("problem type = %q want %q", problem.Type, test.problemType)
		}
	}
}

func TestEnrollmentWritesRequireAuthenticatedPrincipal(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	for _, path := range []string{
		"/api/v1/enrollment-tokens",
		"/api/v1/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3/approval",
		"/api/v1/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3/revocation",
	} {
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d", path, response.Code)
		}
	}
}

func TestEnrollmentErrorsDistinguishInvalidRequestsFromBackendFailures(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/enrollment-tokens", nil)

	response := httptest.NewRecorder()
	server.writeEnrollmentError(response, request, enrollment.ErrInvalidRequest)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid request status = %d", response.Code)
	}

	response = httptest.NewRecorder()
	server.writeEnrollmentError(response, request, errors.New("database unavailable"))
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("backend failure status = %d retry-after = %q", response.Code, response.Header().Get("Retry-After"))
	}
}

func TestCreateEnrollmentTokenRejectsUnboundEndpoint(t *testing.T) {
	server := &Server{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		devAuth:    true,
		enrollment: &enrollment.Service{},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/enrollment-tokens", strings.NewReader(`{
		"workspace_id":"019fc0a4-6d92-765c-a8a1-4af556614cc3",
		"environment":"production",
		"reason":"provision a bound production node"
	}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.createEnrollmentToken(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unbound token status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestOperationsAcceptConfiguredDevelopmentBearer(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "local-development-token-32-characters", 1)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/operations", nil)
	request.Header.Set("Authorization", "Bearer wrong")
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", response.Code)
	}

	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/operations", nil)
	request.Header.Set("Authorization", "Bearer local-development-token-32-characters")
	server.http.Handler.ServeHTTP(response, request)
	if response.Code == http.StatusUnauthorized {
		t.Fatal("configured development bearer was rejected")
	}
}

func TestCreateSimulationRejectsNullBody(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	server.EnableLocalSlice(localslice.New(nil))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/development/simulations", strings.NewReader("null"))
	request.Header.Set("Content-Type", "application/json")
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestDevelopmentRuntimeIsHiddenWhenSimulatorIsDisabled(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/development/runtime", nil)
	response := httptest.NewRecorder()

	server.http.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("development runtime status = %d, want 404", response.Code)
	}
}

func TestDevelopmentRuntimeKeepsProcessMetricsWhenDatabaseIsUnavailable(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://unused@localhost/unused?host=/tmp/ocservia-missing-postgres")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	server := New("127.0.0.1:0", pool, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	server.SetLocalSimulatorEnabled(true)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/development/runtime", nil)
	response := httptest.NewRecorder()

	server.developmentRuntime(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("development runtime status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var body struct {
		KeyStatesAvailable bool `json:"privd_attestation_key_states_available"`
		KeyStates          []struct {
			State string `json:"state"`
		} `json:"privd_attestation_key_states"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.KeyStatesAvailable || len(body.KeyStates) != 3 {
		t.Fatalf("unexpected key state fallback: %+v", body)
	}
}

func TestVersionUsesContractFieldNames(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{Version: "test", Commit: "abc", Role: "api"}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/version", nil))
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["version"] != "test" || body["Version"] != "" {
		t.Fatalf("unexpected response: %v", body)
	}
}

func TestRoutingErrorsUseProblemDetails(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	tests := []struct {
		method string
		path   string
		status int
	}{
		{method: http.MethodPost, path: "/api/v1/livez", status: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/missing", status: http.StatusNotFound},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.status {
			t.Fatalf("%s %s status = %d", test.method, test.path, response.Code)
		}
		if response.Header().Get("Content-Type") != "application/problem+json" {
			t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
		}
		var problem map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
			t.Fatalf("decode problem: %v", err)
		}
		if problem["status"] != float64(test.status) || problem["type"] == "" || problem["title"] == "" {
			t.Fatalf("invalid problem: %v", problem)
		}
	}
}

func TestEnrollmentTTLSecondsRejectsOverflowInputs(t *testing.T) {
	for _, seconds := range []int64{-1, 901, 36028797018963968} {
		if validEnrollmentTTLSeconds(seconds) {
			t.Fatalf("ttl_seconds %d was accepted", seconds)
		}
	}
	for _, seconds := range []int64{0, 1, 900} {
		if !validEnrollmentTTLSeconds(seconds) {
			t.Fatalf("ttl_seconds %d was rejected", seconds)
		}
	}
}

func TestExpectedRevisionRequiresMatchingHeaderOrBody(t *testing.T) {
	version := int64(7)
	for _, test := range []struct {
		header string
		body   *int64
		want   int64
		ok     bool
	}{
		{header: "\"revision-7\"", want: 7, ok: true},
		{body: &version, want: 7, ok: true},
		{header: "\"revision-7\"", body: &version, want: 7, ok: true},
		{header: "revision-6", body: &version, ok: false},
		{header: "*", ok: false},
		{},
	} {
		got, ok := expectedRevision(test.header, test.body)
		if got != test.want || ok != test.ok {
			t.Fatalf("expectedRevision(%q) = %d,%v; want %d,%v", test.header, got, ok, test.want, test.ok)
		}
	}
}

func TestSyntheticCommandRequiresAuthentication(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3/synthetic-commands", strings.NewReader(`{"kind":"noop","expected_version":1}`))
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestControlledOperationRoutesRequireAuthentication(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	for _, path := range []string{
		"/api/v1/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3/sessions/42:disconnect",
		"/api/v1/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3/sessions/42:terminate",
		"/api/v1/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3/ip-bans/192.0.2.9:remove",
		"/api/v1/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3/service:reload",
		"/api/v1/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3/agent-upgrade",
	} {
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("POST %s status = %d", path, response.Code)
		}
	}
}

func TestI14RoutesRequireAuthentication(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3/users/alice/policy"},
		{http.MethodPut, "/api/v1/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3/users/alice/policy"},
		{http.MethodPost, "/api/v1/user-batches"},
		{http.MethodGet, "/api/v1/user-batches/019fc0a4-6d92-765c-a8a1-4af556614cc3"},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d", test.method, test.path, response.Code)
		}
	}
}

func TestBrowserMutationRequiresExactOrigin(t *testing.T) {
	cookiePrincipal := auth.Principal{IdentityID: uuid.Must(uuid.NewV7()), SessionID: uuid.Must(uuid.NewV7()), Issuer: "https://id.example.test"}
	tests := []struct {
		name          string
		trustedOrigin string
		method        string
		origin        string
		fetchSite     string
		principal     auth.Principal
		allowed       bool
	}{
		{"same origin mutation passes", "", http.MethodPost, "https://admin.example.com", "", cookiePrincipal, true},
		{"configured default HTTPS port matches browser origin", "https://admin.example.com:443", http.MethodPost, "https://admin.example.com", "", cookiePrincipal, true},
		{"same origin with same-origin fetch metadata passes", "", http.MethodPost, "https://admin.example.com", "same-origin", cookiePrincipal, true},
		{"same origin with none fetch metadata passes", "", http.MethodPost, "https://admin.example.com", "none", cookiePrincipal, true},
		{"sibling origin is rejected", "", http.MethodPost, "https://app.example.com", "", cookiePrincipal, false},
		{"unknown origin is rejected", "", http.MethodPost, "https://evil.example.net", "", cookiePrincipal, false},
		{"missing origin fails closed", "", http.MethodPost, "", "", cookiePrincipal, false},
		{"port mismatch is rejected", "", http.MethodPost, "https://admin.example.com:8443", "", cookiePrincipal, false},
		{"non-default configured port is rejected for default browser origin", "https://admin.example.com:8443", http.MethodPost, "https://admin.example.com", "", cookiePrincipal, false},
		{"scheme mismatch is rejected", "", http.MethodPost, "http://admin.example.com", "", cookiePrincipal, false},
		{"cross-site fetch metadata is rejected even with correct origin", "", http.MethodPost, "https://admin.example.com", "cross-site", cookiePrincipal, false},
		{"same-site fetch metadata does not replace the exact origin check", "", http.MethodPost, "https://app.example.com", "same-site", cookiePrincipal, false},
		{"cross-site fetch metadata with missing origin is rejected", "", http.MethodPost, "", "cross-site", cookiePrincipal, false},
		{"safe method never requires an origin", "", http.MethodGet, "", "", cookiePrincipal, true},
		{"development principal skips the browser boundary", "", http.MethodPost, "", "", auth.Principal{Subject: "developer", Issuer: "development", BreakGlass: true}, true},
		{"break-glass session passes with the exact origin", "", http.MethodPost, "https://admin.example.com", "", auth.Principal{Subject: "offline", Issuer: "break-glass", BreakGlass: true}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			trustedOrigin := test.trustedOrigin
			if trustedOrigin == "" {
				trustedOrigin = "https://admin.example.com"
			}
			server.EnableBrowserOrigin(trustedOrigin)
			request := httptest.NewRequest(test.method, "/api/v1/enrollment-tokens", nil)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.fetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.fetchSite)
			}
			err := server.validateBrowserMutation(request, test.principal)
			if test.allowed && err != nil {
				t.Fatalf("expected the mutation to pass: %v", err)
			}
			if !test.allowed && !errors.Is(err, errCrossOrigin) {
				t.Fatalf("expected a cross-origin rejection, got %v", err)
			}
		})
	}
}

func TestBrowserMutationFailsClosedWithoutConfiguredOrigin(t *testing.T) {
	server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/enrollment-tokens", nil)
	request.Header.Set("Origin", "https://admin.example.com")
	if err := server.validateBrowserMutation(request, auth.Principal{Issuer: "https://id.example.test"}); !errors.Is(err, errCrossOrigin) {
		t.Fatalf("cookie mutation without a configured origin = %v", err)
	}
}

func TestEnableBrowserOriginRejectsMalformedOrigins(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	for _, origin := range []string{"not a url", "https://admin.example.com/callback", "https://user:pass@admin.example.com", "https://admin.example.com?x=1"} {
		server.EnableBrowserOrigin(origin)
		if server.browserOrigin != "" {
			t.Fatalf("EnableBrowserOrigin(%q) installed %q", origin, server.browserOrigin)
		}
	}
	server.EnableBrowserOrigin("https://Admin.Example.Com")
	if server.browserOrigin != "https://admin.example.com" {
		t.Fatalf("normalized browser origin = %q", server.browserOrigin)
	}
}

func TestRequireOperationAuthPassesDevelopmentBearerWithoutOrigin(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "local-development-token-32-characters", 1)
	server.EnableBrowserOrigin("https://admin.example.com")
	request := httptest.NewRequest(http.MethodPost, "/api/v1/enrollment-tokens", strings.NewReader("{}"))
	request.Header.Set("Authorization", "Bearer local-development-token-32-characters")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code == http.StatusForbidden {
		t.Fatalf("development bearer was stopped by the browser origin boundary: %s", response.Body.String())
	}
}

func TestJSONMediaTypeGate(t *testing.T) {
	server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), devAuth: true, enrollment: &enrollment.Service{}}
	tests := []struct {
		name        string
		contentType string
		body        string
		status      int
	}{
		{"text/plain JSON smuggling is rejected", "text/plain", `{"reason":"test"}`, http.StatusUnsupportedMediaType},
		{"form bodies are rejected", "application/x-www-form-urlencoded", "reason=test", http.StatusUnsupportedMediaType},
		{"multipart bodies are rejected", "multipart/form-data; boundary=x", "--x", http.StatusUnsupportedMediaType},
		{"missing content type is rejected", "", `{"reason":"test"}`, http.StatusUnsupportedMediaType},
		{"application/json passes the gate", "application/json", `{"reason":"test"}`, http.StatusBadRequest},
		{"charset parameter is allowed", "application/json; charset=utf-8", `{"reason":"test"}`, http.StatusBadRequest},
		{"malformed JSON stays invalid", "application/json", "{", http.StatusBadRequest},
		{"unknown fields stay rejected", "application/json", `{"reason":"test","extra":1}`, http.StatusBadRequest},
		{"trailing values stay rejected", "application/json", `{"reason":"test"}{"reason":"second"}`, http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/enrollment-tokens", strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			server.createEnrollmentToken(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.status, response.Body.String())
			}
			if test.status == http.StatusUnsupportedMediaType && !strings.Contains(response.Body.String(), "https://ocservia.dev/problems/unsupported-media-type") {
				t.Fatalf("missing unsupported-media-type problem: %s", response.Body.String())
			}
		})
	}
}

func TestBreakGlassRejectsCrossSiteRequests(t *testing.T) {
	server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), auth: &auth.Service{}}
	server.EnableBrowserOrigin("https://admin.example.com")
	tests := []struct {
		name      string
		origin    string
		fetchSite string
	}{
		{"sibling origin", "https://app.example.com", ""},
		{"cross-site fetch metadata with a spoofed origin", "https://admin.example.com", "cross-site"},
		{"missing origin", "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/break-glass", strings.NewReader(`{"token":"x"}`))
			request.Header.Set("Content-Type", "application/json")
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.fetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.fetchSite)
			}
			response := httptest.NewRecorder()
			server.breakGlass(response, request)
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "https://ocservia.dev/problems/cross-origin-request") {
				t.Fatalf("break-glass boundary status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}
