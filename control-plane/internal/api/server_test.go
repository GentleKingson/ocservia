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
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportevents"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCommonStoreNotFoundProblems(t *testing.T) {
	server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for name, write := range map[string]func(http.ResponseWriter, *http.Request, error){
		"rollout":     server.writeRolloutError,
		"operation":   server.writeOperationError,
		"certificate": writeCertificateError,
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			write(response, httptest.NewRequest(http.MethodGet, "/api/v1/operations/missing", nil), errors.Join(errors.New("store context"), database.ErrNotFound))
			if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "https://ocservia.dev/problems/not-found") {
				t.Fatalf("not-found classification: %d %s", response.Code, response.Body)
			}
		})
	}
}

func TestLiveAndRequestID(t *testing.T) {
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{Version: "test"}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
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
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: true, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/livez", nil)
	request.Host = "127.0.0.1:8080"
	server.http.Handler.ServeHTTP(response, request)
	if response.Header().Get("X-Ocservia-Dev-Subject") != "developer" {
		t.Fatal("development subject was not injected")
	}
}

func TestOperationsRequireAuthenticatedPrincipal(t *testing.T) {
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/operations", "/api/v1/operations/019fc0a4-6d92-765c-a8a1-4af556614cc3"} {
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d", path, response.Code)
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
	testutil.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	if problem.Type != "https://ocservia.dev/problems/capacity-exceeded" {
		t.Fatalf("problem type = %q", problem.Type)
	}
}

func TestOperationBacklogErrorUsesServiceUnavailableProblem(t *testing.T) {
	server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/node/synthetic-commands", nil)
	response := httptest.NewRecorder()
	server.writeOperationError(response, request, operations.ErrBacklogExceeded)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
	var problem struct {
		Type string `json:"type"`
	}
	testutil.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
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
		testutil.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
		if problem.Type != test.problemType {
			t.Fatalf("problem type = %q want %q", problem.Type, test.problemType)
		}
	}
}

func TestEnrollmentWritesRequireAuthenticatedPrincipal(t *testing.T) {
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/v1/node-bootstrap-tokens",
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
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/node-bootstrap-tokens", nil)

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

func TestCreateBootstrapTokenRejectsInvalidExpectedEndpoint(t *testing.T) {
	server := &Server{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		devAuth:    true,
		enrollment: &enrollment.Service{},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/node-bootstrap-tokens", strings.NewReader(`{
		"workspace_id":"019fc0a4-6d92-765c-a8a1-4af556614cc3",
		"expected_endpoint_id":"not-hex",
		"environment":"production",
		"reason":"provision a bound production node"
	}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), principalKey{}, auth.Principal{Subject: "developer", Issuer: "development", BreakGlass: true}))
	response := httptest.NewRecorder()
	server.createNodeBootstrapToken(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid expected_endpoint_id status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestOperationsAcceptConfiguredDevelopmentBearer(t *testing.T) {
	const token = "local-development-token-32-characters"
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: token, EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, host, bearer string
		wantUnauthorized   bool
	}{
		{"wrong token", "127.0.0.1:8080", "wrong", true},
		{"rebound host", "attacker.example:8080", token, true},
		{"loopback", "127.0.0.1:8080", token, false},
		{"localhost", "localhost:8080", token, false},
		{"IPv6 loopback", "[::1]:8080", token, false},
		{"compose web proxy", "web:4173", token, false},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/operations", nil)
		request.Host = test.host
		request.Header.Set("Authorization", "Bearer "+test.bearer)
		server.http.Handler.ServeHTTP(response, request)
		if (response.Code == http.StatusUnauthorized) != test.wantUnauthorized {
			t.Fatalf("%s: status = %d", test.name, response.Code)
		}
		if test.wantUnauthorized && response.Header().Get("X-Ocservia-Dev-Subject") != "" {
			t.Fatalf("%s: development subject was injected", test.name)
		}
	}
}

func TestDevAuthRejectsReboundHost(t *testing.T) {
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: true, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	for host, wantUnauthorized := range map[string]bool{"attacker.example:8080": true, "127.0.0.1:8080": false} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/operations", nil)
		request.Host = host
		server.http.Handler.ServeHTTP(response, request)
		if (response.Code == http.StatusUnauthorized) != wantUnauthorized || (wantUnauthorized && response.Header().Get("X-Ocservia-Dev-Subject") != "") {
			t.Fatalf("%s: status = %d", host, response.Code)
		}
	}
}

func TestCreateSimulationRejectsNullBody(t *testing.T) {
	server, err := NewServer(HTTPConfig{LocalSimulator: true, Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "local-development-token-32-characters", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{TransportEvents: transportevents.NewBackend(nil, nil)}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, host, token string
		want              int
	}{
		{"anonymous", "127.0.0.1:8080", "", http.StatusUnauthorized},
		{"wrong token", "127.0.0.1:8080", "wrong", http.StatusUnauthorized},
		{"rebound host", "attacker.example:8080", "local-development-token-32-characters", http.StatusForbidden},
		{"loopback IPv6", "[::1]:8080", "local-development-token-32-characters", http.StatusBadRequest},
		{"localhost", "localhost:8080", "local-development-token-32-characters", http.StatusBadRequest},
		{"compose web proxy", "web:4173", "local-development-token-32-characters", http.StatusBadRequest},
		{"loopback", "127.0.0.1:8080", "local-development-token-32-characters", http.StatusBadRequest},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/development/simulations", strings.NewReader("null"))
		request.Host = test.host
		request.Header.Set("Content-Type", "application/json")
		if test.token != "" {
			request.Header.Set("Authorization", "Bearer "+test.token)
		}
		server.http.Handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("%s: status = %d, body = %s", test.name, response.Code, response.Body.String())
		}
	}
}

func TestDevelopmentRuntimeIsHiddenWhenSimulatorIsDisabled(t *testing.T) {
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/development/runtime", nil)
	response := httptest.NewRecorder()

	server.http.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("development runtime status = %d, want 404", response.Code)
	}
}

func TestDevelopmentRuntimeKeepsProcessMetricsWhenDatabaseIsUnavailable(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://unused@localhost/unused?host=/tmp/ocservia-missing-postgres")
	testutil.NoError(t, err)
	defer pool.Close()
	server, err := NewServer(HTTPConfig{LocalSimulator: true, Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "local-development-token-32-characters", EventStreams: eventstream.DefaultConfig()}, postgres.WrapPool(pool), BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/development/runtime", nil)
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Authorization", "Bearer local-development-token-32-characters")
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
	testutil.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	if body.KeyStatesAvailable || len(body.KeyStates) != 3 {
		t.Fatalf("unexpected key state fallback: %+v", body)
	}
}

func TestVersionUsesContractFieldNames(t *testing.T) {
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{Version: "test", Commit: "abc"}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/version", nil))
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["version"] != "test" || body["Version"] != "" || body["commit"] != "abc" || body["role"] != "all" {
		t.Fatalf("unexpected response: %v", body)
	}
}

func TestRoutingErrorsUseProblemDetails(t *testing.T) {
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		method string
		path   string
		status int
	}{
		{method: http.MethodPost, path: "/api/v1/livez", status: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/api/v1/auth/login", status: http.StatusMethodNotAllowed},
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
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3/synthetic-commands", strings.NewReader(`{"kind":"noop","expected_version":1}`))
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestControlledOperationRoutesRequireAuthentication(t *testing.T) {
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
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

func TestUserPolicyRoutesRequireAuthentication(t *testing.T) {
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3/users/alice/policy"},
		{http.MethodPut, "/api/v1/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3/users/alice/policy"},
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
			trustedOrigin := test.trustedOrigin
			if trustedOrigin == "" {
				trustedOrigin = "https://admin.example.com"
			}
			config := testHTTPConfig(false)
			config.BrowserOrigin = trustedOrigin
			server := newTestServer(t, config, nil, Modules{}, Authorization{})
			request := httptest.NewRequest(test.method, "/api/v1/node-bootstrap-tokens", nil)
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
	request := httptest.NewRequest(http.MethodPost, "/api/v1/node-bootstrap-tokens", nil)
	request.Header.Set("Origin", "https://admin.example.com")
	if err := server.validateBrowserMutation(request, auth.Principal{Issuer: "local"}); !errors.Is(err, errCrossOrigin) {
		t.Fatalf("cookie mutation without a configured origin = %v", err)
	}
}

func TestHTTPConstructionBrowserOrigin(t *testing.T) {
	for _, origin := range []string{"", "not a url", "https://admin.example.com/callback", "https://user:pass@admin.example.com", "https://admin.example.com?x=1", "https://Admin.Example.Com"} {
		t.Run(origin, func(t *testing.T) {
			config := testHTTPConfig(false)
			config.BrowserOrigin = origin
			server := newTestServer(t, config, nil, Modules{}, Authorization{})
			want := ""
			if origin == "https://Admin.Example.Com" {
				want = "https://admin.example.com"
			}
			if server.browserOrigin != want {
				t.Fatalf("origin %q normalized to %q, want %q", origin, server.browserOrigin, want)
			}
		})
	}
}

func TestRequireOperationAuthPassesDevelopmentBearerWithoutOrigin(t *testing.T) {
	config := testHTTPConfig(false)
	config.BrowserOrigin, config.DevAuthToken = "https://admin.example.com", "local-development-token-32-characters"
	server := newTestServer(t, config, nil, Modules{}, Authorization{})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/node-bootstrap-tokens", strings.NewReader("{}"))
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
			request := httptest.NewRequest(http.MethodPost, "/api/v1/node-bootstrap-tokens", strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			server.createNodeBootstrapToken(response, request)
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
	config := testHTTPConfig(false)
	config.BrowserOrigin = "https://admin.example.com"
	server := newTestServer(t, config, nil, Modules{}, Authorization{Authentication: &auth.Service{}})
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

func TestAPIResponsesAreNotCacheable(t *testing.T) {
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path, want string
	}{
		{"/api/v1/nodes", "no-store"},
		{"/api/v1/not-a-route", "no-store"},
		{"/api/v1/auth/session", "no-store"},
		{"/healthz", ""},
	} {
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if got := response.Header().Get("Cache-Control"); got != test.want {
			t.Fatalf("%s status=%d Cache-Control=%q want %q", test.path, response.Code, got, test.want)
		}
	}
}

func TestOperationHandlersRejectForeignPayloadFields(t *testing.T) {
	// Invalid input must fail at the DTO boundary before any database access.
	server := &Server{operations: operations.NewBackend(nil, 1, nil)}
	for _, tc := range []struct {
		name    string
		body    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"noop message", `{"kind":"noop","message":"unused","expected_version":1}`, server.createSyntheticCommand},
		{"foreign kind", `{"kind":"service_reload","expected_version":1}`, server.createSyntheticCommand},
		{"unknown kind", `{"kind":"method.string","expected_version":1}`, server.createSyntheticCommand},
		{"reload boot", `{"boot_id":"unused","expected_version":1,"reason":"support"}`, server.reloadService},
		{"unban boot", `{"boot_id":"unused","expected_version":1,"reason":"support"}`, func(w http.ResponseWriter, r *http.Request) {
			server.createControlledCommand(w, r, operations.IPBanRemove, "ip_ban.remove", "", "192.0.2.1")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			request.Header.Set("Idempotency-Key", "payload-boundary")
			request.Header.Set("Content-Type", "application/json")
			request.SetPathValue("node_id", "01900000-0000-7000-8000-000000000001")
			response := httptest.NewRecorder()
			tc.handler(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid-request") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
		})
	}
}
