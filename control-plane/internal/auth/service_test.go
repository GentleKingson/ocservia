package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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
