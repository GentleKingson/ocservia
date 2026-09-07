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
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAuthAdmissionLimitsAndRecovery(t *testing.T) {
	now := time.Unix(1000, 0)
	source := netip.MustParseAddr("192.0.2.1")
	budget := newAuthAdmission(2, 3, 1)
	release, _ := budget.admit(source, now, false)
	if release == nil {
		t.Fatal("first request rejected")
	}
	if next, retry := budget.admit(source, now, false); next != nil || retry != time.Second {
		t.Fatal("concurrent request was not bounded")
	}
	release()
	if next, retry := budget.admit(source, now, false); next != nil || retry != time.Minute {
		t.Fatal("source limit was not enforced")
	}
	other := netip.MustParseAddr("192.0.2.2")
	release, _ = budget.admit(other, now, false)
	if release == nil {
		t.Fatal("independent source rejected")
	}
	release()
	if next, _ := budget.admit(netip.MustParseAddr("192.0.2.3"), now, false); next != nil {
		t.Fatal("global rate was not enforced")
	}
	release, _ = budget.admit(source, now.Add(time.Minute), false)
	if release == nil {
		t.Fatal("window did not recover")
	}
	release()
}

func TestAuthAdmissionBoundedStateAndEmergency(t *testing.T) {
	now := time.Unix(1000, 0)
	budget := newAuthAdmission(1, 0, 1)
	for i := 0; i < authSourceCapacity; i++ {
		source := netip.MustParseAddr(fmt.Sprintf("2001:db8::%x", i+1))
		release, _ := budget.admit(source, now, false)
		if release == nil {
			t.Fatal("unexpected admission failure")
		}
		release()
	}
	source := netip.MustParseAddr("192.0.2.1")
	if release, _ := budget.admit(source, now, false); release != nil || len(budget.sources) != authSourceCapacity {
		t.Fatal("source table is not bounded")
	}
	if release, _ := budget.admit(netip.MustParseAddr("2001:db8::1"), now, false); release != nil {
		t.Fatal("live source limit was evicted")
	}
	release, _ := budget.admit(source, now, true)
	if release == nil {
		t.Fatal("source table exhaustion locked out valid emergency credential")
	}
	if next, _ := budget.admit(source, now, true); next != nil {
		t.Fatal("emergency concurrency is unbounded")
	}
	release()
	release, _ = budget.admit(source, now.Add(time.Minute), false)
	if release == nil || len(budget.sources) != 1 {
		t.Fatal("expired source table did not recover")
	}
	release()
}

func TestAuthSourceTrustAndNormalization(t *testing.T) {
	server := &Server{}
	server.ConfigureAuthProxies([]netip.Prefix{netip.MustParsePrefix("10.0.0.2/32")})
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

func TestAuthRouteAdmissionAndIsolation(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	server.auth = &auth.Service{}
	budget := newAuthAdmission(1, 1, 1)
	calls := 0
	handler := server.limitAuthentication(budget, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) })
	for i, want := range []int{http.StatusNoContent, http.StatusTooManyRequests} {
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, "/api/v1/auth/login", nil))
		if response.Code != want || (i == 1 && response.Header().Get("Retry-After") == "") {
			t.Fatalf("status=%d headers=%v", response.Code, response.Header())
		}
	}
	if calls != 1 {
		t.Fatal("limited request reached authentication work")
	}
	// Exhaust callback through the real router; login and break-glass remain separate.
	for i := 0; i < 31; i++ {
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/auth/callback", nil))
		if i == 30 && response.Code != http.StatusTooManyRequests {
			t.Fatalf("callback bypassed route limit: %d", response.Code)
		}
	}
	release, _ := server.breakGlassBudget.admit(netip.MustParseAddr("192.0.2.1"), time.Now(), false)
	if release == nil {
		t.Fatal("OIDC saturation consumed break-glass budget")
	}
	release()
}

func TestBreakGlassInvalidTokensLimitedBeforeService(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", 1)
	var err error
	server.auth, err = auth.New(context.Background(), &pgxpool.Pool{}, auth.Config{Issuer: "https://idp.example", ClientID: "client", ClientSecret: "secret", RedirectURL: "https://admin.example.com/api/v1/auth/callback", SessionKey: make([]byte, 32), SessionTTL: time.Hour, BreakGlassEnabled: true, BreakGlassTokenHash: auth.TokenHash("correct")})
	if err != nil {
		t.Fatal(err)
	}
	server.EnableBrowserOrigin("https://admin.example.com")
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
