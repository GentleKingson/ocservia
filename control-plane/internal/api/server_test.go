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

	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
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
