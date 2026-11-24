package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAuthAdmissionLimitsAndRecovery(t *testing.T) {
	now := time.Unix(1000, 0)
	source := netip.MustParseAddr("192.0.2.1")
	budget := newAuthAdmission(2, 3, 1)
	release, _ := budget.admit(source, now)
	if release == nil {
		t.Fatal("first request rejected")
	}
	if next, retry := budget.admit(source, now); next != nil || retry != time.Second {
		t.Fatal("concurrent request was not bounded")
	}
	release()
	// The concurrency rejection did no authentication work and spent no budget.
	release, _ = budget.admit(source, now)
	if release == nil {
		t.Fatal("concurrency rejection consumed the source budget")
	}
	release()
	if next, retry := budget.admit(source, now); next != nil || retry != time.Minute {
		t.Fatal("source limit was not enforced")
	}
	other := netip.MustParseAddr("192.0.2.2")
	release, _ = budget.admit(other, now)
	if release == nil {
		t.Fatal("independent source rejected")
	}
	release()
	if next, _ := budget.admit(netip.MustParseAddr("192.0.2.3"), now); next != nil {
		t.Fatal("global rate was not enforced")
	}
	release, _ = budget.admit(source, now.Add(time.Minute))
	if release == nil {
		t.Fatal("window did not recover")
	}
	release()
}

func TestAuthAdmissionBoundedStateAndIPv6Slash64(t *testing.T) {
	now := time.Unix(1000, 0)
	budget := newAuthAdmission(2, 0, 1)
	// Addresses in the same IPv6 /64 share one source slot and rate budget.
	release, _ := budget.admit(netip.MustParseAddr("2001:db8::1"), now)
	if release == nil {
		t.Fatal("first IPv6 attempt rejected")
	}
	release()
	release, _ = budget.admit(netip.MustParseAddr("2001:db8::2"), now)
	if release == nil {
		t.Fatal("second IPv6 attempt in same /64 rejected")
	}
	release()
	if next, _ := budget.admit(netip.MustParseAddr("2001:db8::3"), now); next != nil || len(budget.sources) != 1 {
		t.Fatalf("IPv6 /64 normalization failed: next=%v len=%d", next != nil, len(budget.sources))
	}

	// Distinct IPv6 /64 prefixes fill the bounded table cleanly, then fail closed.
	for i := 1; i < authSourceCapacity; i++ {
		source := netip.MustParseAddr(fmt.Sprintf("2001:db8:0:%x::1", i))
		release, _ := budget.admit(source, now)
		if release == nil {
			t.Fatalf("unexpected admission failure at /64 index %d", i)
		}
		release()
	}
	source := netip.MustParseAddr("192.0.2.1")
	if release, _ := budget.admit(source, now); release != nil || len(budget.sources) != authSourceCapacity {
		t.Fatal("source table is not bounded")
	}
	if release, _ := budget.admit(netip.MustParseAddr("2001:db8::1"), now); release != nil {
		t.Fatal("live source limit was evicted")
	}
	release, _ = budget.admit(source, now.Add(time.Minute))
	if release == nil || len(budget.sources) != 1 {
		t.Fatal("expired source table did not recover")
	}
	release()
}

func TestAuthSourceTrustAndNormalization(t *testing.T) {
	config := testHTTPConfig(false)
	config.AuthTrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.2/32")}
	server := newTestServer(t, config, nil, Modules{}, Authorization{})
	for _, test := range []struct {
		name, peer string
		header     []string
		want       string
	}{
		{"direct spoof", "192.0.2.1:1000", []string{"198.51.100.1"}, "192.0.2.1"},
		{"trusted proxy", "10.0.0.2:1000", []string{"198.51.100.1"}, "198.51.100.1"},
		{"mapped client", "10.0.0.2:1000", []string{"::ffff:198.51.100.1"}, "198.51.100.1"},
		{"mapped peer", "[::ffff:192.0.2.1]:1000", nil, "192.0.2.1"},
		{"duplicate", "10.0.0.2:1000", []string{"198.51.100.1", "198.51.100.2"}, "10.0.0.2"},
		{"list", "10.0.0.2:1000", []string{"198.51.100.1, 198.51.100.2"}, "10.0.0.2"},
		{"zone", "10.0.0.2:1000", []string{"fe80::1%eth0"}, "10.0.0.2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.RemoteAddr = test.peer
			request.Header["X-Ocservia-Client-Ip"] = test.header
			request.Header.Set("X-Forwarded-For", "203.0.113.1")
			if got := server.authSource(request).String(); got != test.want {
				t.Fatalf("source=%s want=%s", got, test.want)
			}
		})
	}
}

func TestBreakGlassInvalidTokensLimitedBeforeService(t *testing.T) {
	service, err := auth.NewBackend(postgres.WrapPool(&pgxpool.Pool{}), auth.Config{SessionKey: make([]byte, 32), SessionTTL: time.Hour, BreakGlassEnabled: true, BreakGlassTokenHash: auth.TokenHash("correct")})
	if err != nil {
		t.Fatal(err)
	}
	config := testHTTPConfig(false)
	config.BrowserOrigin = "https://admin.example.com"
	server := newTestServer(t, config, nil, Modules{}, Authorization{Authentication: service})
	for i := 0; i < 6; i++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/break-glass", strings.NewReader(`{"token":"wrong"}`))
		request.Header.Set("Origin", "https://admin.example.com")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, request)
		want := http.StatusUnauthorized
		if i == 5 {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("attempt=%d status=%d body=%s", i, response.Code, response.Body)
		}
	}
}

func TestBreakGlassEmptyTokenIsRejected(t *testing.T) {
	service, err := auth.NewBackend(postgres.WrapPool(&pgxpool.Pool{}), auth.Config{SessionKey: make([]byte, 32), SessionTTL: time.Hour, BreakGlassEnabled: true, BreakGlassTokenHash: auth.TokenHash("correct")})
	if err != nil {
		t.Fatal(err)
	}
	config := testHTTPConfig(false)
	config.BrowserOrigin = "https://admin.example.com"
	server := newTestServer(t, config, nil, Modules{}, Authorization{Authentication: service})
	for _, body := range []string{`{}`, `{"token":""}`, `{"token":"   "}`} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/break-glass", strings.NewReader(body))
		request.Header.Set("Origin", "https://admin.example.com")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || response.Header().Get("Set-Cookie") != "" || response.Header().Get("Content-Type") != "application/problem+json" {
			t.Fatalf("body=%s status=%d headers=%v", body, response.Code, response.Header())
		}
	}
}

func TestInstallationRegistrationLimitedBeforeWork(t *testing.T) {
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/node-bootstrap/register"
	send := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"node_id":"018f0c2e-7b1a-7c3d-8e9f-0123456789ab","registration":{"version":1,"key_id":"k","public_key":"!","controller_nonce":"!","credential_context_sha256":"!","signature":"!"}}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, request)
		return response
	}
	// Disabled installation keeps its 404 and spends no budget.
	for i := 0; i < 31; i++ {
		if response := send(); response.Code != http.StatusNotFound {
			t.Fatalf("disabled attempt=%d status=%d", i, response.Code)
		}
	}
	server = newTestServer(t, testHTTPConfig(false), nil, Modules{Enrollment: enrollment.NewBackend(postgres.WrapPool(&pgxpool.Pool{}), "", "test", nil)}, Authorization{})
	for i := 0; i < 31; i++ {
		response := send()
		want := http.StatusBadRequest
		if i == 30 {
			want = http.StatusTooManyRequests
		}
		if response.Code != want || (want == http.StatusTooManyRequests && response.Header().Get("Retry-After") == "") {
			t.Fatalf("attempt=%d status=%d headers=%v body=%s", i, response.Code, response.Header(), response.Body)
		}
	}
	release, _ := server.localLoginBudget.admit(netip.MustParseAddr("192.0.2.1"), time.Now())
	if release == nil {
		t.Fatal("registration saturation consumed the login budget")
	}
	release()
}

func TestBreakGlassValidAndInvalidShareRateLimitZeroOracle(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:db@127.0.0.1:1/test")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	service, err := auth.NewBackend(postgres.WrapPool(pool), auth.Config{SessionKey: make([]byte, 32), SessionTTL: time.Hour, BreakGlassEnabled: true, BreakGlassTokenHash: auth.TokenHash("correct")})
	if err != nil {
		t.Fatal(err)
	}
	config := testHTTPConfig(false)
	config.BrowserOrigin = "https://admin.example.com"
	server := newTestServer(t, config, nil, Modules{}, Authorization{Authentication: service})

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/break-glass", strings.NewReader(`{"token":"wrong"}`))
		req.RemoteAddr = "198.51.100.10:1234"
		req.Header.Set("Origin", "https://admin.example.com")
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("attempt=%d status=%d body=%s", i, resp.Code, resp.Body)
		}
	}
	// Valid token from the same saturated source must also receive 429 (no token oracle).
	validReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/break-glass", strings.NewReader(`{"token":"correct"}`))
	validReq.RemoteAddr = "198.51.100.10:1234"
	validReq.Header.Set("Origin", "https://admin.example.com")
	validReq.Header.Set("Content-Type", "application/json")
	validResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(validResp, validReq)
	if validResp.Code != http.StatusTooManyRequests || validResp.Header().Get("Retry-After") == "" {
		t.Fatalf("valid break-glass token bypassed source rate limit (oracle): status=%d headers=%v body=%s", validResp.Code, validResp.Header(), validResp.Body)
	}
	// Saturating break-glass must not spend the local login budget.
	release, _ := server.localLoginBudget.admit(netip.MustParseAddr("198.51.100.10"), time.Now())
	if release == nil {
		t.Fatal("break-glass saturation consumed localLoginBudget")
	}
	release()
}
