package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
)

const authTestOrigin = "https://admin.example.test"

func newAuthHTTPServer(t *testing.T, pool *pgxpool.Pool, local bool, issuer string) *Server {
	t.Helper()
	cfg := auth.Config{LocalEnabled: local, SessionKey: make([]byte, 32), SessionTTL: time.Hour}
	if issuer != "" {
		cfg.Issuer, cfg.ClientID, cfg.ClientSecret = issuer, "client", "secret"
		cfg.RedirectURL = authTestOrigin + "/api/v1/auth/callback"
	}
	if pool == nil {
		pool = &pgxpool.Pool{}
	}
	service, err := auth.New(context.Background(), pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := New("127.0.0.1:0", pool, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, 15*time.Second, false, "", 1)
	s.auth = service
	s.EnableBrowserOrigin(authTestOrigin)
	return s
}

func authHTTPRequest(s *Server, method, path, body, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1/auth/"+path, strings.NewReader(body))
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Request-ID", "r6-http-request")
	w := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(w, r)
	return w
}

func TestAuthHTTPMethodsAndDisabledRoutes(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, oidc := range []bool{false, true} {
			t.Run(fmt.Sprintf("local=%t/oidc=%t", local, oidc), func(t *testing.T) {
				issuer := ""
				if oidc {
					issuer = "https://idp.example.test"
				}
				s := newAuthHTTPServer(t, nil, local, issuer)
				w := authHTTPRequest(s, "GET", "methods", "", "")
				if w.Code != 200 || strings.TrimSpace(w.Body.String()) != fmt.Sprintf(`{"local":%t,"oidc":%t}`, local, oidc) {
					t.Fatalf("methods: %d %s", w.Code, w.Body)
				}
				if !local {
					for i := 0; i < 7; i++ {
						if w := authHTTPRequest(s, "POST", "login", `{}`, ""); w.Code != 404 {
							t.Fatalf("disabled local: %d", w.Code)
						}
					}
				}
				if !oidc {
					for _, path := range []string{"login", "callback"} {
						for i := 0; i < 32; i++ {
							if w := authHTTPRequest(s, "GET", path, "", ""); w.Code != 404 {
								t.Fatalf("disabled OIDC %s: %d", path, w.Code)
							}
						}
					}
				}
			})
		}
	}
	s := newAuthHTTPServer(t, nil, false, "")
	s.auth = nil
	if w := authHTTPRequest(s, "GET", "methods", "", ""); strings.TrimSpace(w.Body.String()) != `{"local":false,"oidc":false}` {
		t.Fatal(w.Body)
	}
}

func TestLocalLoginHTTPBoundary(t *testing.T) {
	for _, test := range []struct {
		name, body, origin, mediaType, fetchSite string
		want                                     int
	}{
		{"missing origin", `{}`, "", "application/json", "", 403},
		{"normalized origin", `{}`, "https://ADMIN.example.test:443", "application/json", "", 400},
		{"sibling origin", `{}`, "https://sibling.example.test", "application/json", "same-site", 403},
		{"different port", `{}`, authTestOrigin + ":444", "application/json", "", 403},
		{"cross-site metadata", `{}`, authTestOrigin, "application/json", "cross-site", 403},
		{"wrong media", `{}`, authTestOrigin, "text/plain", "", 415},
		{"unknown field", `{"username":"a","password":"b","extra":true}`, authTestOrigin, "application/json", "", 400},
		{"trailing JSON", `{} {}`, authTestOrigin, "application/json", "", 400},
		{"malformed", `{`, authTestOrigin, "application/json", "", 400},
		{"null", `null`, authTestOrigin, "application/json", "", 400},
		{"missing password", `{"username":"a"}`, authTestOrigin, "application/json", "", 400},
		{"wrong type", `{"username":1,"password":"b"}`, authTestOrigin, "application/json", "", 400},
		{"body limit", `{"username":"a","password":"` + strings.Repeat("p", 8192) + `"}`, authTestOrigin, "application/json", "", 400},
		{"username limit", `{"username":"` + strings.Repeat("a", 129) + `","password":"b"}`, authTestOrigin, "application/json", "", 401},
		{"password byte limit", `{"username":"a","password":"` + strings.Repeat("\u00e9", 513) + `"}`, authTestOrigin, "application/json", "", 401},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newAuthHTTPServer(t, nil, true, "")
			logs := captureAuthLogs(s)
			r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(test.body))
			r.Header.Set("Origin", test.origin)
			r.Header.Set("Content-Type", test.mediaType)
			r.Header.Set("Sec-Fetch-Site", test.fetchSite)
			w := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, r)
			if w.Code != test.want || len(w.Result().Cookies()) != 0 {
				t.Fatalf("status=%d body=%s cookies=%v", w.Code, w.Body, w.Result().Cookies())
			}
			reason := "invalid_request"
			if test.want == 403 {
				reason = "origin_rejected"
			}
			if test.want == 401 {
				reason = "credentials_rejected"
			}
			assertAuthLog(t, logs, "local", "rejected", reason)
		})
	}
}

func TestLocalLoginHTTPAdmission(t *testing.T) {
	for _, kind := range []string{"source", "global", "concurrent"} {
		t.Run(kind, func(t *testing.T) {
			s := newAuthHTTPServer(t, nil, true, "")
			attempts := 6
			if kind == "global" {
				attempts = 121
			}
			if kind == "concurrent" {
				for i := 0; i < 4; i++ {
					s.localLoginBudget.active <- struct{}{}
				}
				attempts = 1
			}
			for i := 0; i < attempts; i++ {
				r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"username":"","password":""}`))
				r.Header.Set("Origin", authTestOrigin)
				r.Header.Set("Content-Type", "application/json")
				if kind == "global" {
					r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i+1)
				}
				w := httptest.NewRecorder()
				s.http.Handler.ServeHTTP(w, r)
				want := 401
				if i == attempts-1 {
					want = 429
				}
				if w.Code != want || (want == 429 && w.Header().Get("Retry-After") == "") {
					t.Fatalf("attempt=%d status=%d body=%s", i, w.Code, w.Body)
				}
			}
		})
	}
}

func TestAuthHTTPLoginLogoutIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var issuer, nonce, challenge string
	var secrets []string
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("code") == "code-bait-r6-rejected" {
				http.Error(w, `{"error":"invalid_grant","error_description":"upstream-secret-bait-r6"}`, 400)
				return
			}
			digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(digest[:]) != challenge || r.Form.Get("redirect_uri") != authTestOrigin+"/api/v1/auth/callback" {
				t.Error("OIDC PKCE or redirect changed")
				http.Error(w, "invalid grant", 400)
				return
			}
			claims, _ := json.Marshal(map[string]any{"iss": issuer, "aud": "client", "sub": "http-operator", "nonce": nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
			signed, err := signer.Sign(claims)
			if err != nil {
				t.Error(err)
				return
			}
			token, err := signed.CompactSerialize()
			if err != nil {
				t.Error(err)
				return
			}
			secrets = append(secrets, token)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-token-bait-r6", "refresh_token": "refresh-token-bait-r6", "token_type": "Bearer", "id_token": token})
		default:
			http.NotFound(w, r)
		}
	}))
	defer idp.Close()
	issuer = idp.URL
	for _, mode := range []string{"local", "oidc", "both"} {
		t.Run(mode, func(t *testing.T) {
			provider := issuer
			if mode == "local" {
				provider = ""
			}
			s := newAuthHTTPServer(t, pool, mode != "oidc", provider)
			logs := captureAuthLogs(s)
			defer func() {
				assertNoAuthSecrets(t, logs, secrets...)
				assertNoAuthSecrets(t, logs, "http-test-password", "access-token-bait-r6", "refresh-token-bait-r6", "code-bait-r6-rejected", "upstream-secret-bait-r6")
			}()
			logout := func(cookie *http.Cookie) {
				t.Helper()
				assertAuthenticationAuthorizationParity(t, s, pool, cookie)
				if cookie.Name != auth.SessionCookieName || !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode {
					t.Fatal("nonstandard session cookie")
				}
				if _, err := s.auth.Authenticate(context.Background(), cookie); err != nil {
					t.Fatal(err)
				}
				secrets = append(secrets, cookie.Value)
				r := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
				r.Header.Set("Origin", authTestOrigin)
				r.AddCookie(cookie)
				w := httptest.NewRecorder()
				s.http.Handler.ServeHTTP(w, r)
				if w.Code != 204 || len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].MaxAge != -1 {
					t.Fatalf("logout: %d %s", w.Code, w.Body)
				}
				if _, err := s.auth.Authenticate(context.Background(), cookie); err == nil {
					t.Fatal("revoked session accepted")
				}
			}
			if mode != "oidc" {
				username := "http-" + uuid.NewString()
				if _, err := s.auth.CreateLocalCredential(context.Background(), username, "http-test-password"); err != nil {
					t.Fatal(err)
				}
				wrong := authHTTPRequest(s, "POST", "login", fmt.Sprintf(`{"username":%q,"password":"wrong"}`, username), authTestOrigin)
				wrongLog := assertAuthLog(t, logs, "local", "rejected", "credentials_rejected")
				missing := authHTTPRequest(s, "POST", "login", `{"username":"missing-http-user","password":"wrong"}`, authTestOrigin)
				missingLog := assertAuthLog(t, logs, "local", "rejected", "credentials_rejected")
				if wrongLog["account_ref"] != s.auth.LocalAccountRef(username) || missingLog["account_ref"] != s.auth.LocalAccountRef("missing-http-user") {
					t.Fatal("failed account correlation missing")
				}
				if wrong.Code != 401 || missing.Code != 401 || wrong.Body.String() != missing.Body.String() || len(wrong.Result().Cookies()) != 0 || len(missing.Result().Cookies()) != 0 {
					t.Fatal("credential failures differ")
				}
				w := authHTTPRequest(s, "POST", "login", fmt.Sprintf(`{"username":%q,"password":"http-test-password"}`, username), authTestOrigin)
				if w.Code != 204 || len(w.Result().Cookies()) != 1 {
					t.Fatalf("local login: %d %s", w.Code, w.Body)
				}
				actor, err := s.auth.Authenticate(context.Background(), w.Result().Cookies()[0])
				if err != nil {
					t.Fatal(err)
				}
				record := assertAuthLog(t, logs, "local", "succeeded", "session_created")
				if record["identity_id"] != actor.IdentityID.String() || record["request_id"] != w.Header().Get("X-Request-ID") {
					t.Fatal("success correlation differs")
				}
				assertNoAuthSecrets(t, logs, username)
				logout(w.Result().Cookies()[0])
			}
			if mode != "local" {
				do := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
					r := httptest.NewRequest("GET", "/api/v1/auth/"+path, nil)
					r.Header.Set("X-Request-ID", "r6-oidc-request")
					r = r.WithContext(context.WithValue(r.Context(), oauth2.HTTPClient, idp.Client()))
					if cookie != nil {
						r.AddCookie(cookie)
					}
					w := httptest.NewRecorder()
					s.http.Handler.ServeHTTP(w, r)
					return w
				}
				w := do("login", nil)
				assertAuthLog(t, logs, "oidc", "started", "redirect_created")
				if w.Code != 302 || len(w.Result().Cookies()) != 1 {
					t.Fatalf("OIDC start: %d %s", w.Code, w.Body)
				}
				location, err := url.Parse(w.Header().Get("Location"))
				if err != nil {
					t.Fatal(err)
				}
				query := location.Query()
				nonce, challenge = query.Get("nonce"), query.Get("code_challenge")
				if query.Get("code_challenge_method") != "S256" || nonce == "" || query.Get("state") == "" {
					t.Fatal("OIDC protections missing")
				}
				cookie := w.Result().Cookies()[0]
				secrets = append(secrets, cookie.Value, query.Get("state"), nonce)
				if rejected := do("callback?state=wrong&code=test", cookie); rejected.Code != 401 {
					t.Fatal("invalid state accepted")
				}
				assertAuthLog(t, logs, "oidc", "rejected", "state_rejected")
				if rejected := do("callback?state="+url.QueryEscape(query.Get("state"))+"&code=code-bait-r6-rejected", cookie); rejected.Code != 401 {
					t.Fatal("upstream failure accepted")
				}
				assertAuthLog(t, logs, "oidc", "rejected", "callback_rejected")
				t.Run("session-write-failure", func(t *testing.T) {
					ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
					if ownerURL == "" {
						t.Skip("isolated owner database URL required")
					}
					ctx := context.Background()
					owner, err := pgxpool.New(ctx, ownerURL)
					if err != nil {
						t.Fatal(err)
					}
					defer owner.Close()
					constraint := "r6_oidc_" + strings.ReplaceAll(uuid.NewString(), "-", "")
					if _, err := owner.Exec(ctx, `ALTER TABLE auth_sessions ADD CONSTRAINT `+constraint+` CHECK (false) NOT VALID`); err != nil {
						t.Fatal(err)
					}
					defer func() {
						if _, err := owner.Exec(ctx, `ALTER TABLE auth_sessions DROP CONSTRAINT `+constraint); err != nil {
							t.Error(err)
						}
					}()
					var before, after int
					if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions`).Scan(&before); err != nil {
						t.Fatal(err)
					}
					previousLogs := len(authLogRecords(t, logs))
					failed := do("callback?state="+url.QueryEscape(query.Get("state"))+"&code=test", cookie)
					if failed.Code != 401 || failed.Header().Get("Location") != "" || !strings.Contains(failed.Body.String(), "oidc-callback-rejected") {
						t.Fatal("session failure changed public callback contract")
					}
					for _, c := range failed.Result().Cookies() {
						if c.Name != auth.LoginCookieName || c.MaxAge != -1 {
							t.Fatal("failed session issued a cookie")
						}
					}
					assertAuthLog(t, logs, "oidc", "unavailable", "infrastructure_failure")
					if len(authLogRecords(t, logs)) != previousLogs+1 {
						t.Fatal("duplicate session failure result")
					}
					if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions`).Scan(&after); err != nil || after != before {
						t.Fatal("failed session write did not roll back")
					}
					assertNoAuthSecrets(t, logs, constraint, cookie.Value, query.Get("state"), "access-token-bait-r6", "refresh-token-bait-r6", "upstream-secret-bait-r6")
				})
				w = do("callback?state="+url.QueryEscape(query.Get("state"))+"&code=test", cookie)
				if w.Code != 302 {
					t.Fatalf("OIDC callback: %d %s", w.Code, w.Body)
				}
				var session *http.Cookie
				for _, c := range w.Result().Cookies() {
					if c.Name == auth.SessionCookieName {
						session = c
					}
				}
				if session == nil {
					t.Fatal("missing OIDC session")
				}
				actor, err := s.auth.Authenticate(context.Background(), session)
				if err != nil {
					t.Fatal(err)
				}
				record := assertAuthLog(t, logs, "oidc", "succeeded", "session_created")
				if record["identity_id"] != actor.IdentityID.String() || record["request_id"] != "r6-oidc-request" {
					t.Fatal("OIDC success correlation differs")
				}
				logout(session)
				fresh := do("callback?state="+url.QueryEscape(query.Get("state"))+"&code=test", cookie)
				if fresh.Code != 302 {
					t.Fatalf("existing identity callback: %d", fresh.Code)
				}
				for _, c := range fresh.Result().Cookies() {
					if c.Name == auth.SessionCookieName {
						session = c
					}
				}
				ctx := context.Background()
				if _, err := s.auth.Authenticate(ctx, session); err != nil {
					t.Fatalf("fresh session before disable: %v", err)
				}
				var identityID uuid.UUID
				if err := pool.QueryRow(ctx, `UPDATE identities SET disabled_at=now() WHERE issuer=$1 AND subject='http-operator' RETURNING id`, issuer).Scan(&identityID); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = pool.Exec(ctx, `UPDATE identities SET disabled_at=NULL WHERE id=$1`, identityID) })
				request := httptest.NewRequest("GET", "/api/v1/workspaces", nil)
				request.AddCookie(session)
				response := httptest.NewRecorder()
				s.http.Handler.ServeHTTP(response, request)
				if response.Code != 401 {
					t.Fatalf("disabled session business API: %d", response.Code)
				}
				var before, after int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE identity_id=$1`, identityID).Scan(&before); err != nil {
					t.Fatal(err)
				}
				rejected := do("callback?state="+url.QueryEscape(query.Get("state"))+"&code=test", cookie)
				assertAuthLog(t, logs, "oidc", "rejected", "callback_rejected")
				if rejected.Code != 401 || rejected.Header().Get("Location") != "" || !strings.Contains(rejected.Body.String(), "oidc-callback-rejected") {
					t.Fatalf("disabled callback protocol: %d %s", rejected.Code, rejected.Body)
				}
				for _, c := range rejected.Result().Cookies() {
					if c.Name == auth.SessionCookieName || c.Name != auth.LoginCookieName || c.MaxAge != -1 {
						t.Fatalf("unexpected rejected callback cookie: %s", c.Name)
					}
				}
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE identity_id=$1`, identityID).Scan(&after); err != nil || after != before {
					t.Fatalf("disabled callback sessions: %d -> %d, %v", before, after, err)
				}
			}
		})
	}
}

// Exercise the same HTTP authorization path with cookies from actual Local and
// OIDC logins, rather than injecting a principal past the session middleware.
func assertAuthenticationAuthorizationParity(t *testing.T, s *Server, pool *pgxpool.Pool, cookie *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	actor, err := s.auth.Authenticate(ctx, cookie)
	if err != nil || actor.BreakGlass {
		t.Fatalf("ordinary principal: %+v %v", actor, err)
	}
	s.EnableAuthorization(s.auth, rbac.New(pool), approvals.New(pool), nil)
	workspaceID, targetID, approverID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'P6 parity',$2,now(),now())`, workspaceID, "parity-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM role_bindings WHERE workspace_id=$1`, workspaceID) }()
	for _, id := range []uuid.UUID{targetID, approverID} {
		if _, err := pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'https://parity.example',$2,now(),now())`, id, id.String()); err != nil {
			t.Fatal(err)
		}
	}
	call := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/api/v1/"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", authTestOrigin)
		r.Header.Set("X-Workspace-ID", workspaceID.String())
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d want %d: %s", actor.Issuer, path, w.Code, want, w.Body)
		}
		return w
	}
	binding := fmt.Sprintf(`{"identity_id":%q,"workspace_id":%q,"role":"SecurityAdmin","resource_type":"workspace","reason":"P6 parity"}`, targetID, workspaceID)
	call("GET", "nodes", "", 403)
	call("POST", "role-bindings", binding, 403)
	for id, role := range map[uuid.UUID]string{actor.IdentityID: "PlatformAdmin", approverID: "SecurityAdmin"} {
		if _, err := pool.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,$4,'workspace',now())`, uuid.Must(uuid.NewV7()), id, workspaceID, role); err != nil {
			t.Fatal(err)
		}
	}
	if w := call("GET", "workspaces", "", 200); !strings.Contains(w.Body.String(), workspaceID.String()) {
		t.Fatalf("authorized workspace missing: %s", w.Body)
	}
	call("POST", "role-bindings", binding, 400)
	w := call("POST", "approval-requests", fmt.Sprintf(`{"action":"role_binding.elevate","resource_type":"role_binding","resource_id":%q,"reason":"P6 parity","ttl_seconds":600,"role_binding":{"identity_id":%q,"role":"SecurityAdmin","resource_type":"workspace"}}`, targetID, targetID), 201)
	var approval approvals.Approval
	if err := json.Unmarshal(w.Body.Bytes(), &approval); err != nil {
		t.Fatal(err)
	}
	call("POST", "approval-requests/"+approval.ID.String()+":approve", fmt.Sprintf(`{"reason":"self","expected_request_hash":%q}`, approval.RequestHash), 403)
	// The independent approver is a database fixture; the requester uses the
	// real login cookie for every read, approval request and protected mutation.
	approverSession := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now())`, approverSession, approverID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.approvals.Approve(ctx, approvals.Decision{ApprovalID: approval.ID, ApproverID: approverID, SessionID: approverSession, RequestID: uuid.NewString(), Reason: "independent review", ExpectedRequestHash: approval.RequestHash}); err != nil {
		t.Fatal(err)
	}
	approvedBinding := strings.TrimSuffix(binding, "}") + fmt.Sprintf(`,"approval_id":%q}`, approval.ID)
	call("POST", "role-bindings", approvedBinding, 201)
	call("POST", "role-bindings", approvedBinding, 400)
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND actor_id=$2 AND source_session_id=$3 AND action='role_binding.create' AND result='succeeded'`, workspaceID, actor.IdentityID.String(), actor.SessionID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("principal/session audit parity: %d %v", count, err)
	}
}
