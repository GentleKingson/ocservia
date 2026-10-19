package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
)

func TestOptionalAuthenticationProviders(t *testing.T) {
	ctx := context.Background()
	for _, local := range []bool{false, true} {
		s, err := NewBackend(postgres.WrapPool(&pgxpool.Pool{}), Config{LocalEnabled: local, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.BeginLogin(ctx); !errors.Is(err, ErrOIDCDisabled) {
			t.Fatalf("disabled OIDC login: %v", err)
		}
		if !local {
			if _, _, err := s.AuthenticateLocal(ctx, "alice", "password"); !errors.Is(err, ErrLocalDisabled) {
				t.Fatalf("disabled local login: %v", err)
			}
			if _, err := s.CreateLocalCredential(ctx, "alice", "password"); !errors.Is(err, ErrLocalDisabled) {
				t.Fatalf("disabled local provisioning: %v", err)
			}
		}
	}
	for _, issuer := range []string{"local", "break-glass"} {
		if _, err := NewBackend(postgres.WrapPool(&pgxpool.Pool{}), Config{Issuer: issuer, ClientID: "client", RedirectURL: "https://console.example/callback", SessionKey: make([]byte, 32), SessionTTL: time.Hour}); err == nil {
			t.Fatalf("reserved identity source accepted as OIDC: %s", issuer)
		}
	}
}

func TestOIDCTLSAndIssuerOutagesFailClosed(t *testing.T) {
	tlsIssuer := httptest.NewTLSServer(http.NotFoundHandler())
	defer tlsIssuer.Close()
	service, err := NewBackend(postgres.WrapPool(&pgxpool.Pool{}), Config{Issuer: tlsIssuer.URL, ClientID: "client", ClientSecret: "secret", RedirectURL: "https://console.example/api/v1/auth/callback", SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if location, cookie, err := service.BeginLogin(ctx); err == nil || location != "" || cookie != nil {
		t.Fatalf("untrusted issuer TLS did not fail closed: location=%q cookie=%v err=%v", location, cookie, err)
	}

	unavailable := httptest.NewTLSServer(http.NotFoundHandler())
	issuer := unavailable.URL
	unavailable.Close()
	service, err = NewBackend(postgres.WrapPool(&pgxpool.Pool{}), Config{Issuer: issuer, ClientID: "client", ClientSecret: "secret", RedirectURL: "https://console.example/api/v1/auth/callback", SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if location, cookie, err := service.BeginLogin(ctx); err == nil || location != "" || cookie != nil {
		t.Fatalf("unavailable issuer did not fail closed: location=%q cookie=%v err=%v", location, cookie, err)
	}
}

func TestBeginLoginUsesPKCES256StateNonceAndSecureCookie(t *testing.T) {
	service, err := NewBackend(postgres.WrapPool(&pgxpool.Pool{}), Config{Issuer: "https://idp.example", ClientID: "client", ClientSecret: "secret", RedirectURL: "https://console.example/api/v1/auth/callback", SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	service.discover = func(context.Context) (oauth2.Config, *oidc.IDTokenVerifier, error) {
		return oauth2.Config{ClientID: "client", RedirectURL: "https://console.example/api/v1/auth/callback", Endpoint: oauth2.Endpoint{AuthURL: "https://idp.example/authorize", TokenURL: "https://idp.example/token"}}, nil, nil
	}
	location, cookie, err := service.BeginLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	for name := range map[string]bool{"state": true, "nonce": true, "code_challenge": true} {
		if len(query.Get(name)) < 32 {
			t.Fatalf("%s missing", name)
		}
	}
	if query.Get("code_challenge_method") != "S256" {
		t.Fatalf("challenge method = %q", query.Get("code_challenge_method"))
	}
	if query.Get("response_type") != "code" || query.Get("redirect_uri") != "https://console.example/api/v1/auth/callback" {
		t.Fatalf("authorization query = %s", parsed.RawQuery)
	}
	if cookie.Name != LoginCookieName || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("insecure login cookie: %#v", cookie)
	}
	if _, _, err := service.CompleteLogin(context.Background(), query.Get("state")+"x", "code", cookie); err != ErrOIDCState {
		t.Fatalf("state mismatch error = %v", err)
	}
}

func TestNewRejectsNonHTTPSRedirectAndWeakSessionKey(t *testing.T) {
	_, err := NewBackend(postgres.WrapPool(&pgxpool.Pool{}), Config{Issuer: "https://idp.example", ClientID: "client", ClientSecret: "secret", RedirectURL: "http://console.example/callback", SessionKey: make([]byte, 31), SessionTTL: time.Hour})
	if err == nil {
		t.Fatal("unsafe OIDC configuration accepted")
	}
}

func TestValidBreakGlassTokenIsOnlyCredentialAdmission(t *testing.T) {
	service := &Service{breakGlassEnabled: true, breakGlassTokenHash: TokenHash("correct")}
	if !service.ValidBreakGlassToken("correct") || service.ValidBreakGlassToken("wrong") {
		t.Fatal("incorrect emergency credential admission")
	}
	service.breakGlassEnabled = false
	if service.ValidBreakGlassToken("correct") {
		t.Fatal("disabled emergency credential accepted")
	}
}

func TestOIDCProviderCacheCoalescesAndRecovers(t *testing.T) {
	service, err := NewBackend(postgres.WrapPool(&pgxpool.Pool{}), Config{Issuer: "https://idp.example", ClientID: "client", ClientSecret: "secret", RedirectURL: "https://console.example/api/v1/auth/callback", SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	service.now = func() time.Time { return now }
	var calls, joins atomic.Int32
	service.discovery.joined = func() { joins.Add(1) }
	fail := true
	release := make(chan struct{})
	service.discover = func(ctx context.Context) (oauth2.Config, *oidc.IDTokenVerifier, error) {
		calls.Add(1)
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > oidcDiscoveryTimeout {
			t.Errorf("Discovery is not bounded: %v %t", deadline, ok)
		}
		<-release
		if ctx.Err() != nil {
			t.Errorf("Discovery inherited a caller cancellation: %v", ctx.Err())
		}
		if fail {
			return oauth2.Config{}, nil, errors.New("issuer unavailable")
		}
		return oauth2.Config{ClientID: "client"}, nil, nil
	}

	// A failed first Discovery is shared by concurrent callers and not cached.
	errs := make(chan error, 3)
	for range 3 {
		go func() { _, _, err := service.provider(context.Background()); errs <- err }()
	}
	waitForCalls(t, &joins, 3)
	close(release)
	for range 3 {
		if err := <-errs; err == nil {
			t.Fatal("failed Discovery returned a provider")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent Discovery calls = %d, want 1", calls.Load())
	}

	// A cancelled caller stops waiting without failing the shared Discovery.
	fail = false
	release = make(chan struct{})
	cancelled, cancel := context.WithCancel(context.Background())
	go func() { _, _, err := service.provider(cancelled); errs <- err }()
	waitForCalls(t, &calls, 2)
	waiter := make(chan error, 1)
	go func() { _, _, err := service.provider(context.Background()); waiter <- err }()
	waitForCalls(t, &joins, 5)
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller error = %v", err)
	}
	close(release)
	if err := <-waiter; err != nil {
		t.Fatalf("cancellation failed the shared Discovery: %v", err)
	}

	// Recovery is cached until the TTL ends.
	if config, _, err := service.provider(context.Background()); err != nil || config.ClientID != "client" || calls.Load() != 2 {
		t.Fatalf("cached provider: %v calls=%d", err, calls.Load())
	}
	now = now.Add(oidcDiscoveryTTL)
	if _, _, err := service.provider(context.Background()); err != nil || calls.Load() != 3 {
		t.Fatalf("expired provider was not refreshed: %v calls=%d", err, calls.Load())
	}
}

func waitForCalls(t *testing.T, calls *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("Discovery calls = %d, want %d", calls.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}
