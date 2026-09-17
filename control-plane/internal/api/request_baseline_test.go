package api

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
)

func TestHTTPPathBaseline(t *testing.T) {
	s := baselineServer(t, false)
	for _, tc := range []struct {
		method, path, allow, kind string
		status                    int
	}{
		{"GET", "/missing", "", "not-found", 404},
		{"OPTIONS", "/missing", "", "not-found", 404},
		{"POST", "/livez", "GET", "method-not-allowed", 405},
		{"GET", "/api/v1/local-users/a:unknown", "", "not-found", 404},
		{"POST", "/api/v1/nodes/invalid/sessions/42:unknown", "", "not-found", 404},
		{"POST", "/api/v1/nodes/invalid/sessions/42:terminate", "", "unauthenticated", 401},
		{"POST", "/api/v1/nodes/invalid/users/alice:enable", "", "unauthenticated", 401},
		{"POST", "/api/v1/nodes/invalid/users/alice:rotate-password", "", "unauthenticated", 401},
		{"POST", "/api/v1/certificates/invalid:revoke", "", "unauthenticated", 401},
		{"POST", "/api/v1/certificates/invalid:p12", "", "unauthenticated", 401},
		{"POST", "/api/v1/certificates/invalid:unknown", "GET", "method-not-allowed", 405},
		{"GET", "/livez/", "", "not-found", 404},
		{"GET", "/api/v1/nodes//invalid", "", "not-found", 404},
		{"GET", "/api/v1/%6eodes/invalid", "", "unauthenticated", 401},
		{"GET", "/api/v1/nodes/a%2Fb", "", "not-found", 404},
		{"POST", "/api/v1/nodes/invalid/users/alice%3Adisable", "", "unauthenticated", 401},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			r := baselineRequest(tc.method, tc.path, strings.NewReader("broken JSON"))
			w := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, r)
			title, detail := "Resource not found", "the requested resource does not exist"
			if tc.status == 401 {
				title, detail = "Authentication required", "operation state requires an authenticated principal"
			}
			if tc.status == 405 {
				title, detail = "Method not allowed", "the requested method is not supported"
			}
			assertBaselineProblem(t, w, r.URL.Path, tc.status, tc.kind, title, detail)
			if w.Header().Get("Allow") != tc.allow {
				t.Fatalf("Allow = %q", w.Header().Get("Allow"))
			}
		})
	}
	// These are intentionally not normalized into routeErrors' Problem format.
	for _, tc := range []struct {
		path, location, contentType, body string
		status                            int
	}{
		{"/api/v1/nodes/invalid/", "", "text/plain; charset=utf-8", "404 page not found\n", 404},
		{"//api/v1/nodes/invalid", "/api/v1/nodes/invalid", "text/html; charset=utf-8", "<a href=\"/api/v1/nodes/invalid\">Temporary Redirect</a>.\n\n", 307},
	} {
		w := httptest.NewRecorder() // no client redirect following
		s.http.Handler.ServeHTTP(w, baselineRequest("GET", tc.path, nil))
		if w.Code != tc.status || w.Header().Get("Location") != tc.location || w.Header().Get("Content-Type") != tc.contentType || w.Body.String() != tc.body {
			t.Fatalf("%s: %d %v %q", tc.path, w.Code, w.Header(), w.Body.String())
		}
	}
}

type baselineReadError struct{}

func (baselineReadError) Read([]byte) (int, error) { return 0, errors.New("fixture read failure") }

func TestHTTPStrictJSONBaseline(t *testing.T) {
	for _, tc := range []struct {
		name, media, body, detail string
		status                    int
	}{
		{"missing-media", "", "{}", "Content-Type must be application/json", 415},
		{"text", "text/plain", "{}", "Content-Type must be application/json", 415},
		{"bad-media", "application/json; broken", "{}", "Content-Type must be application/json", 415},
		{"empty", "application/json", "", "request body is invalid", 400},
		{"syntax", "application/json", "{", "request body is invalid", 400},
		{"utf8", "application/json", "{\"reason\":\"\xff\"}", "request body must be valid UTF-8 JSON", 400},
		{"unknown", "application/json", "{\"unknown\":1}", "request body is invalid", 400},
		{"second-value", "application/json", "{} {}", "request body must contain one JSON value", 400},
		{"trailing-junk", "application/json", "{} x", "request body must contain one JSON value", 400},
		{"null", "application/json", "null", "workspace_id must be UUIDv7", 400},
		{"parameters", "application/json; charset=utf-8", "{}", "workspace_id must be UUIDv7", 400},
		{"duplicate-key", "application/json", "{\"reason\":\"a\",\"reason\":\"b\"}", "workspace_id must be UUIDv7", 400},
		{"at-limit", "application/json", "{}" + strings.Repeat(" ", 1022), "workspace_id must be UUIDv7", 400},
		{"over-limit", "application/json", "{}" + strings.Repeat(" ", 1023), "request body must be valid UTF-8 JSON", 400},
		{"read-error", "application/json", "", "request body must be valid UTF-8 JSON", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := baselineServer(t, true)
			s.EnableEnrollment(&enrollment.Service{}, nil)
			var body io.Reader = strings.NewReader(tc.body)
			if tc.name == "read-error" {
				body = baselineReadError{}
			}
			r := baselineRequest("POST", "/api/v1/enrollment-tokens", body)
			r.Header.Set("Content-Type", tc.media)
			w := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, r)
			kind, title := "invalid-request", "Invalid request"
			if tc.status == 415 {
				kind, title = "unsupported-media-type", "Unsupported media type"
			}
			assertBaselineProblem(t, w, r.URL.Path, tc.status, kind, title, tc.detail)
		})
	}
}

func TestHTTPConfigPlanValidationBaseline(t *testing.T) {
	for _, route := range []string{"/api/v1/nodes/" + baselineID + "/config-plans", "/api/v1/config-plans/" + baselineID + "/apply"} {
		for _, key := range []string{"", " \t ", " fixture-key "} {
			s := baselineServer(t, true)
			s.EnableConfigPlans(&configplan.Service{})
			r := baselineRequest("POST", route, strings.NewReader("{"))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", key)
			w := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, r)
			if strings.TrimSpace(key) == "" {
				assertBaselineProblem(t, w, route, 400, "idempotency-key-required", "Idempotency key is required", "Idempotency-Key must be provided")
			} else {
				assertBaselineProblem(t, w, route, 400, "invalid-request", "Invalid request", "request body is invalid")
			}
		}
	}
	for _, id := range []string{baselineID, strings.ReplaceAll(baselineID, "-", ""), "urn:uuid:" + baselineID, "{" + baselineID + "}", "invalid", "519fc0a4-6d92-465c-a8a1-4af556614cc3"} {
		s := baselineServer(t, true)
		s.EnableConfigPlans(&configplan.Service{})
		// Directly setting RawPath is unnecessary: the request URL parser retains
		// the baseline's accepted UUID spellings in the segment.
		r := baselineRequest("POST", "/api/v1/nodes/"+id+"/config-plans", nil)
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		if id == "invalid" || strings.HasPrefix(id, "519") {
			assertBaselineProblem(t, w, r.URL.Path, 400, "invalid-id", "Invalid identifier", "node_id must be UUIDv7")
		} else {
			assertBaselineProblem(t, w, r.URL.Path, 400, "idempotency-key-required", "Idempotency key is required", "Idempotency-Key must be provided")
		}
	}
}

func TestHTTPRequestContextBaseline(t *testing.T) {
	for _, id := range []string{" retained ", "", strings.Repeat("x", 129), "invalid\nvalue"} {
		s := baselineServer(t, true)
		var logs bytes.Buffer
		s.logger = slog.New(slog.NewJSONHandler(&logs, nil))
		r := baselineRequest("GET", "/missing", nil)
		r.Header.Set("X-Request-ID", id)
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		got := w.Header().Get("X-Request-ID")
		if id == " retained " {
			if got != "retained" {
				t.Fatal(got)
			}
		} else if raw, err := hex.DecodeString(got); err != nil || len(raw) != 16 {
			t.Fatalf("generated request ID %q", got)
		}
		if w.Header().Get("X-Ocservia-Dev-Subject") != "developer" {
			t.Fatal("missing development marker")
		}
		var log map[string]any
		if err := json.Unmarshal(logs.Bytes(), &log); err != nil {
			t.Fatal(err)
		}
		if log["request_id"] != got || log["trace_id"] != "0123456789abcdef0123456789abcdef" || log["method"] != "GET" || log["path"] != "/missing" || log["msg"] != "http request" {
			t.Fatalf("log correlation: %v", log)
		}
	}
}

func TestHTTPDisabledServicesBaseline(t *testing.T) {
	s := baselineServer(t, true)
	for _, tc := range []struct {
		method, path, kind, title, detail string
		status                            int
	}{
		{"GET", "/api/v1/nodes", "telemetry-unavailable", "Telemetry unavailable", "the telemetry read model is unavailable", 503},
		{"GET", "/api/v1/nodes/invalid", "telemetry-unavailable", "Telemetry unavailable", "the telemetry read model is unavailable", 503},
		{"GET", "/api/v1/nodes/invalid/sessions", "telemetry-unavailable", "Telemetry unavailable", "the telemetry read model is unavailable", 503},
		{"GET", "/api/v1/nodes/invalid/ip-bans", "telemetry-unavailable", "Telemetry unavailable", "the telemetry read model is unavailable", 503},
		{"GET", "/api/v1/nodes/invalid/telemetry", "telemetry-unavailable", "Telemetry unavailable", "the telemetry read model is unavailable", 503},
		{"POST", "/api/v1/nodes/invalid/config-plans", "service-unavailable", "Service unavailable", "configuration plan service is unavailable", 503},
		{"POST", "/api/v1/enrollment-tokens", "not-found", "Resource not found", "enrollment is not enabled", 404},
		{"GET", "/api/v1/operations", "not-found", "Resource not found", "the requested resource does not exist", 404},
		{"GET", "/api/v1/operations/summary", "not-found", "Resource not found", "the requested resource does not exist", 404},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, baselineRequest(tc.method, tc.path, strings.NewReader("{")))
			assertBaselineProblem(t, w, tc.path, tc.status, tc.kind, tc.title, tc.detail)
		})
	}
}
