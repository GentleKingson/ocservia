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

	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
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
		if response.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("%s missing bearer challenge", path)
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
