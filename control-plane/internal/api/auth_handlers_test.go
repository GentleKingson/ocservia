package api

import (
	"context"
	"fmt"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const authTestOrigin = "https://admin.example.test"

func newAuthHTTPServer(t *testing.T, pool *pgxpool.Pool, local bool, httpConfigs ...HTTPConfig) *Server {
	t.Helper()
	cfg := auth.Config{LocalEnabled: local, SessionKey: make([]byte, 32), SessionTTL: time.Hour}
	if pool == nil {
		pool = &pgxpool.Pool{}
	}
	service, err := auth.NewBackend(postgres.WrapPool(pool), cfg)
	testutil.NoError(t, err)
	config := testHTTPConfig(false)
	if len(httpConfigs) > 0 {
		config = httpConfigs[0]
	}
	return newTestServer(t, config, postgres.WrapPool(pool), Modules{}, Authorization{Authentication: service})
}

func authHTTPRequest(s *Server, method, path, body, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1/auth/"+path, strings.NewReader(body))
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Request-ID", "auth-http-request")
	w := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(w, r)
	return w
}

func TestAuthHTTPDisabledRoutes(t *testing.T) {
	s := newAuthHTTPServer(t, nil, false)
	for i := 0; i < 7; i++ {
		if w := authHTTPRequest(s, "POST", "login", `{}`, ""); w.Code != 404 {
			t.Fatalf("disabled local: %d", w.Code)
		}
	}
	if w := authHTTPRequest(s, "GET", "methods", "", ""); w.Code != 404 {
		t.Fatalf("retired methods: %d", w.Code)
	}
	if w := authHTTPRequest(s, "GET", "callback", "", ""); w.Code != 404 {
		t.Fatalf("retired callback: %d", w.Code)
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
		{"body limit", `{"username":"a","password":"` + strings.Repeat("p", 8192) + `"}`, authTestOrigin, "application/json", "", 413},
		{"username limit", `{"username":"` + strings.Repeat("a", 129) + `","password":"b"}`, authTestOrigin, "application/json", "", 401},
		{"password byte limit", `{"username":"a","password":"` + strings.Repeat("\u00e9", 513) + `"}`, authTestOrigin, "application/json", "", 401},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newAuthHTTPServer(t, nil, true)
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
			s := newAuthHTTPServer(t, nil, true)
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
	testutil.NoError(t, err)
	defer pool.Close()

	s := newAuthHTTPServer(t, pool, true)
	logs := captureAuthLogs(s)
	defer assertNoAuthSecrets(t, logs, "http-test-password")

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
	cookie := w.Result().Cookies()[0]
	actor, err := s.auth.Authenticate(context.Background(), cookie)
	testutil.NoError(t, err)
	record := assertAuthLog(t, logs, "local", "succeeded", "session_created")
	if record["identity_id"] != actor.IdentityID.String() || record["request_id"] != w.Header().Get("X-Request-ID") {
		t.Fatal("success correlation differs")
	}
	assertNoAuthSecrets(t, logs, username, cookie.Value)

	assertAuthenticationAuthorizationParity(t, s, pool, cookie)
	if cookie.Name != auth.SessionCookieName || !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("nonstandard session cookie")
	}
	r := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	r.Header.Set("Origin", authTestOrigin)
	r.AddCookie(cookie)
	logoutResp := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(logoutResp, r)
	if logoutResp.Code != 204 || len(logoutResp.Result().Cookies()) != 1 || logoutResp.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("logout: %d %s", logoutResp.Code, logoutResp.Body)
	}
	if _, err := s.auth.Authenticate(context.Background(), cookie); err == nil {
		t.Fatal("revoked session accepted")
	}
}

func assertAuthenticationAuthorizationParity(t *testing.T, s *Server, pool *pgxpool.Pool, cookie *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	actor, err := s.auth.Authenticate(ctx, cookie)
	if err != nil || actor.BreakGlass {
		t.Fatalf("ordinary principal: %+v %v", actor, err)
	}
	workspaceID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Authentication authorization parity',$2,now(),now())`, workspaceID, "parity-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM workspace_admins WHERE workspace_id=$1`, workspaceID)
	}()
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
	call("GET", "nodes", "", 403)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_admins(workspace_id,identity_id,created_at) VALUES($1,$2,now())`, workspaceID, actor.IdentityID); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "workspaces", "", 200); !strings.Contains(w.Body.String(), workspaceID.String()) {
		t.Fatalf("authorized workspace missing: %s", w.Body)
	}
	call("GET", "nodes", "", 503) // authorized past authz guard; Telemetry reader nil in this server
}
