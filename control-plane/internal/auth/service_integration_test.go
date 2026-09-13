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

	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOIDCAuthorizationCodePKCEIntegration(t *testing.T) {
	for _, suffix := range []string{"", "/", "/tenant/"} {
		t.Run("issuer="+suffix, func(t *testing.T) { testOIDCAuthorizationCodePKCE(t, suffix) })
	}
}

func testOIDCAuthorizationCodePKCE(t *testing.T, suffix string) {
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
	var discoveryMismatch, unavailable bool
	discoveryRequests := 0
	var expected loginState
	redirectURL := "https://console.example/api/v1/auth/callback"
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case strings.TrimSuffix(suffix, "/") + "/.well-known/openid-configuration":
			discoveryRequests++
			if unavailable {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			advertised := issuer
			if discoveryMismatch {
				advertised = strings.TrimSuffix(issuer, "/")
				if advertised == issuer {
					advertised += "/"
				}
			}
			w.Header().Set("Content-Type", "application/json")
			base := strings.TrimSuffix(issuer, suffix)
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": advertised, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token", "jwks_uri": base + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
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
			overrides := map[string]any{}
			switch r.Form.Get("code") {
			case "bad-issuer":
				other := strings.TrimSuffix(issuer, "/")
				if other == issuer {
					other += "/"
				}
				overrides["iss"] = other
			case "bad-audience":
				overrides["aud"] = "other-client"
			case "expired":
				overrides["exp"] = time.Now().Add(-time.Hour).Unix()
			}
			token := signIDToken(t, key, issuer, nonce, overrides)
			if r.Form.Get("code") == "bad-signature" {
				parts := strings.Split(token, ".")
				signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
				signature[0] ^= 1
				token = parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(signature)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "expires_in": 60, "id_token": token})
		default:
			http.NotFound(w, r)
		}
	}))
	defer idp.Close()
	issuer = idp.URL + suffix
	// A preexisting no-slash identity must retain its ID and role. For slash
	// issuers, the same sub/profile under the other issuer must stay separate.
	legacyID, workspaceID, bindingID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,email,display_name,created_at,updated_at) VALUES($1,$2,'operator-1','operator@example.test','Operator',now(),now())`, legacyID, strings.TrimSuffix(issuer, "/")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'R5',$2,now(),now())`, workspaceID, workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,created_at) VALUES($1,$2,$3,'Viewer',now())`, bindingID, legacyID, workspaceID); err != nil {
		t.Fatal(err)
	}

	service, err := NewBackend(postgres.WrapPool(pool), Config{Issuer: issuer, ClientID: "client", ClientSecret: "secret", RedirectURL: redirectURL, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	location, loginCookie, err := service.BeginLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	discoveryMismatch = true
	if _, cookie, err := service.BeginLogin(ctx); err == nil || cookie != nil || !strings.Contains(err.Error(), "did not match the issuer") {
		t.Fatalf("Discovery mismatch accepted or wrong failure: %v", err)
	}
	discoveryMismatch = false
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
	if (principal.IdentityID == legacyID) != (suffix == "") {
		t.Fatalf("issuer identity boundary changed: legacy=%s principal=%s", legacyID, principal.IdentityID)
	}
	var roleOwner uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT identity_id FROM role_bindings WHERE id=$1 AND role_name='Viewer'`, bindingID).Scan(&roleOwner); err != nil || roleOwner != legacyID {
		t.Fatalf("legacy role changed: %s %v", roleOwner, err)
	}
	if authenticated, err := service.Authenticate(ctx, sessionCookie); err != nil || authenticated.IdentityID != principal.IdentityID {
		t.Fatalf("authenticate established session: %#v, %v", authenticated, err)
	}
	unavailable = true
	if _, _, err := service.BeginLogin(ctx); err == nil {
		t.Fatal("new login succeeded while the OIDC issuer was unavailable")
	}
	if authenticated, err := service.Authenticate(ctx, sessionCookie); err != nil || authenticated.IdentityID != principal.IdentityID {
		t.Fatalf("bounded existing session failed during issuer outage: %#v, %v", authenticated, err)
	}
	unavailable = false

	_, loginCookie, err = service.BeginLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.open(loginCookie.Value, &expected); err != nil {
		t.Fatal(err)
	}
	if cookie, _, err := service.CompleteLogin(ctx, expected.State+"wrong", "valid-code", loginCookie); !errors.Is(err, ErrOIDCState) || cookie != nil {
		t.Fatalf("invalid state accepted: %v", err)
	}
	for _, code := range []string{"bad-nonce", "bad-signature", "bad-issuer", "bad-audience", "expired"} {
		if cookie, _, err := service.CompleteLogin(ctx, expected.State, code, loginCookie); err == nil || cookie != nil {
			t.Fatalf("invalid token %s accepted: %v", code, err)
		}
	}
	second, existing, err := service.CompleteLogin(ctx, expected.State, "valid-code", loginCookie)
	if err != nil || existing.IdentityID != principal.IdentityID || second == nil {
		t.Fatalf("existing identity login: %+v, %v", existing, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identities SET disabled_at=now() WHERE id=$1`, principal.IdentityID); err != nil {
		t.Fatal(err)
	}
	denied, _, loginErr := service.CompleteLogin(ctx, expected.State, "valid-code", loginCookie)
	var sessions, identities int
	var disabled bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM auth_sessions WHERE identity_id=$1), (SELECT count(*) FROM identities WHERE issuer=$2 AND subject=$3), disabled_at IS NOT NULL FROM identities WHERE id=$1`, principal.IdentityID, issuer, principal.Subject).Scan(&sessions, &identities, &disabled); err != nil {
		t.Fatal(err)
	}
	_, authErr := service.Authenticate(ctx, sessionCookie)
	t.Logf("disabled OIDC: cookie issued=%t, sessions=%d, identities=%d, disabled=%t, existing session rejected=%t", denied != nil, sessions, identities, disabled, errors.Is(authErr, ErrUnauthenticated))
	if !errors.Is(loginErr, ErrUnauthenticated) || denied != nil || sessions != 2 || identities != 1 || !disabled || !errors.Is(authErr, ErrUnauthenticated) {
		t.Fatalf("disabled identity login: %v", loginErr)
	}
	if discoveryRequests != 12 {
		t.Fatalf("real Discovery requests = %d, want 12", discoveryRequests)
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

func signIDToken(t *testing.T, key *rsa.PrivateKey, issuer, nonce string, overrides map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test", "typ": "JWT"})
	values := map[string]any{"iss": issuer, "aud": "client", "sub": "operator-1", "email": "operator@example.test", "name": "Operator", "nonce": nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()}
	for key, value := range overrides {
		values[key] = value
	}
	claims, _ := json.Marshal(values)
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
	service, err := NewBackend(postgres.WrapPool(pool), Config{LocalEnabled: true, Issuer: "https://idp.example", ClientID: "client", ClientSecret: "secret", RedirectURL: "https://console.example/api/v1/auth/callback", SessionKey: make([]byte, 32), SessionTTL: time.Hour, BreakGlassEnabled: true, BreakGlassTokenHash: TokenHash(token)})
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
	var bounded bool
	if err := pool.QueryRow(ctx, `SELECT expires_at-created_at=interval '15 minutes' FROM auth_sessions WHERE id=$1`, principal.SessionID).Scan(&bounded); err != nil || !bounded {
		t.Fatalf("break-glass TTL changed: %t %v", bounded, err)
	}
	var alerts, audits int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM security_alerts WHERE source_session_id=$1 AND severity='critical'),(SELECT count(*) FROM audit_events WHERE workspace_id=$2 AND source_session_id=$1 AND action='break_glass.use')`, principal.SessionID, workspaceID).Scan(&alerts, &audits); err != nil || alerts != 1 || audits != 1 {
		t.Fatalf("break-glass alert/audit = %d/%d, %v", alerts, audits, err)
	}
	if _, _, err := service.BreakGlass(ctx, token, "replay"); !errors.Is(err, ErrBreakGlassRotationDue) {
		t.Fatalf("credential reuse error = %v", err)
	}
}
