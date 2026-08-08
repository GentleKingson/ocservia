package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
)

func TestOIDCAuthorizationCodePKCEIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var issuer string
	var expected loginState
	redirectURL := "https://console.example/api/v1/auth/callback"
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/keys":
			writeJWKS(t, w, &key.PublicKey)
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code_verifier") != expected.Verifier || r.Form.Get("redirect_uri") != redirectURL {
				t.Errorf("unexpected token request: %v", r.Form)
				http.Error(w, "invalid grant", http.StatusBadRequest)
				return
			}
			nonce := expected.Nonce
			if r.Form.Get("code") == "bad-nonce" {
				nonce += "-replayed"
			}
			token := signIDToken(t, key, issuer, nonce)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "expires_in": 60, "id_token": token})
		default:
			http.NotFound(w, r)
		}
	}))
	defer idp.Close()
	issuer = idp.URL

	service, err := New(ctx, pool, Config{Issuer: issuer, ClientID: "client", ClientSecret: "secret", RedirectURL: redirectURL, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	service.discover = func(context.Context) (oauth2.Config, *oidc.IDTokenVerifier, error) {
		oauth := oauth2.Config{ClientID: "client", ClientSecret: "secret", RedirectURL: redirectURL, Endpoint: oauth2.Endpoint{AuthURL: issuer + "/authorize", TokenURL: issuer + "/token"}, Scopes: []string{oidc.ScopeOpenID}}
		verifier := oidc.NewVerifier(issuer, oidc.NewRemoteKeySet(ctx, issuer+"/keys"), &oidc.Config{ClientID: "client"})
		return oauth, verifier, nil
	}

	location, loginCookie, err := service.BeginLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.open(loginCookie.Value, &expected); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(location, "code_challenge_method=S256") {
		t.Fatalf("authorization URL omitted PKCE S256: %s", location)
	}
	sessionCookie, principal, err := service.CompleteLogin(ctx, expected.State, "valid-code", loginCookie)
	if err != nil {
		t.Fatal(err)
	}
	if principal.Subject != "operator-1" || principal.Issuer != issuer {
		t.Fatalf("unexpected principal: %#v", principal)
	}
	if authenticated, err := service.Authenticate(ctx, sessionCookie); err != nil || authenticated.IdentityID != principal.IdentityID {
		t.Fatalf("authenticate established session: %#v, %v", authenticated, err)
	}
	service.discover = func(context.Context) (oauth2.Config, *oidc.IDTokenVerifier, error) {
		return oauth2.Config{}, nil, errors.New("issuer unavailable")
	}
	if _, _, err := service.BeginLogin(ctx); err == nil {
		t.Fatal("new login succeeded while the OIDC issuer was unavailable")
	}
	if authenticated, err := service.Authenticate(ctx, sessionCookie); err != nil || authenticated.IdentityID != principal.IdentityID {
		t.Fatalf("bounded existing session failed during issuer outage: %#v, %v", authenticated, err)
	}
	service.discover = func(context.Context) (oauth2.Config, *oidc.IDTokenVerifier, error) {
		oauth := oauth2.Config{ClientID: "client", ClientSecret: "secret", RedirectURL: redirectURL, Endpoint: oauth2.Endpoint{AuthURL: issuer + "/authorize", TokenURL: issuer + "/token"}, Scopes: []string{oidc.ScopeOpenID}}
		verifier := oidc.NewVerifier(issuer, oidc.NewRemoteKeySet(ctx, issuer+"/keys"), &oidc.Config{ClientID: "client"})
		return oauth, verifier, nil
	}

	_, loginCookie, err = service.BeginLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.open(loginCookie.Value, &expected); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.CompleteLogin(ctx, expected.State, "bad-nonce", loginCookie); err == nil {
		t.Fatal("OIDC nonce replay was accepted")
	}
}

func writeJWKS(t *testing.T, w http.ResponseWriter, publicKey *rsa.PublicKey) {
	t.Helper()
	exponent := make([]byte, 4)
	binary.BigEndian.PutUint32(exponent, uint32(publicKey.E))
	exponent = exponent[1:]
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(publicKey.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(exponent)}}}); err != nil {
		t.Error(err)
	}
}

func signIDToken(t *testing.T, key *rsa.PrivateKey, issuer, nonce string) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": issuer, "aud": "client", "sub": "operator-1", "email": "operator@example.test", "name": "Operator", "nonce": nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
	unsigned := fmt.Sprintf("%s.%s", base64.RawURLEncoding.EncodeToString(header), base64.RawURLEncoding.EncodeToString(claims))
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestBreakGlassAlertsAuditsAndRequiresRotationIntegration(t *testing.T) {
	url := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	workspaceID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'break glass',$2,now(),now())`, workspaceID, "break-glass-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	token := "offline-emergency-credential-with-high-entropy"
	service, err := New(ctx, pool, Config{Issuer: "https://idp.example", ClientID: "client", ClientSecret: "secret", RedirectURL: "https://console.example/api/v1/auth/callback", SessionKey: make([]byte, 32), SessionTTL: time.Hour, BreakGlassEnabled: true, BreakGlassTokenHash: TokenHash(token)})
	if err != nil {
		t.Fatal(err)
	}
	cookie, principal, err := service.BreakGlass(ctx, token, "break-glass-request")
	if err != nil {
		t.Fatal(err)
	}
	if !principal.BreakGlass || !cookie.Secure || !cookie.HttpOnly {
		t.Fatal("break-glass session is not hardened")
	}
	var alerts, audits int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM security_alerts WHERE source_session_id=$1),(SELECT count(*) FROM audit_events WHERE workspace_id=$2 AND source_session_id=$1 AND action='break_glass.use')`, principal.SessionID, workspaceID).Scan(&alerts, &audits); err != nil || alerts != 1 || audits != 1 {
		t.Fatalf("break-glass alert/audit = %d/%d, %v", alerts, audits, err)
	}
	if _, _, err := service.BreakGlass(ctx, token, "replay"); !errors.Is(err, ErrBreakGlassRotationDue) {
		t.Fatalf("credential reuse error = %v", err)
	}
}
