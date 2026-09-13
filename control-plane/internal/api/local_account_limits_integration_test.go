package api

import (
	"bytes"
	"context"
	"fmt"
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

func TestLocalAccountHTTPSharedLimitsIntegration(t *testing.T) {
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
	otherPool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer otherPool.Close()
	servers := []*Server{newAuthHTTPServer(t, pool, true, ""), newAuthHTTPServer(t, otherPool, true, "")}
	logs := []*bytes.Buffer{captureAuthLogs(servers[0]), captureAuthLogs(servers[1])}
	name := "r2-http-" + uuid.NewString()
	missing := "missing-" + name
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM local_auth_attempts WHERE username IN ($1,$2)`, name, missing)
	}()
	const password = "r2 http test password"
	if _, err := servers[0].auth.CreateLocalCredential(ctx, name, password); err != nil {
		t.Fatal(err)
	}
	request := func(i int, username, pass string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, username, pass)))
		r.Header.Set("Origin", authTestOrigin)
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i+1)
		w := httptest.NewRecorder()
		servers[i%2].http.Handler.ServeHTTP(w, r)
		return w
	}
	var body string
	for i := 0; i < 7; i++ {
		for _, username := range []string{name, missing} {
			w := request(i, " "+strings.ToUpper(username)+" ", "wrong")
			reason := "credentials_rejected"
			if i >= 5 {
				reason = "account_limited"
			}
			record := assertAuthLog(t, logs[i%2], "local", "rejected", reason)
			if record["account_ref"] != servers[0].auth.LocalAccountRef(username) {
				t.Fatal("cross-instance account log correlation differs")
			}
			if body == "" {
				body = w.Body.String()
			}
			if w.Code != 401 || w.Body.String() != body || w.Header().Get("Retry-After") != "" || len(w.Result().Cookies()) != 0 {
				t.Fatalf("existence/cooldown response differs: %d %s", w.Code, w.Body)
			}
			if i == 4 {
				var failures int
				if err := pool.QueryRow(ctx, `SELECT failures FROM local_auth_attempts WHERE username=$1`, username).Scan(&failures); err != nil || failures != 5 {
					t.Fatalf("cross-IP/instance/normalization failures=%d err=%v", failures, err)
				}
				if _, err := pool.Exec(ctx, `UPDATE local_auth_attempts SET blocked_until=clock_timestamp()+interval '5 minutes' WHERE username=$1`, username); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if w := request(8, name, password); w.Code != 401 || w.Body.String() != body {
		t.Fatal("correct password bypassed cooldown")
	}
	// Account saturation must not enter OIDC or the emergency credential path.
	token := uuid.NewString()
	servers[0].auth, err = auth.NewBackend(postgres.WrapPool(pool), auth.Config{LocalEnabled: true, Issuer: "https://idp.example.test", ClientID: "client", RedirectURL: authTestOrigin + "/api/v1/auth/callback", SessionKey: make([]byte, 32), SessionTTL: time.Hour, BreakGlassEnabled: true, BreakGlassTokenHash: auth.TokenHash(token)})
	if err != nil {
		t.Fatal(err)
	}
	if w := authHTTPRequest(servers[0], "GET", "callback", "", ""); w.Code != 401 || !strings.Contains(w.Body.String(), "oidc-callback-rejected") {
		t.Fatalf("OIDC was account-limited: %d %s", w.Code, w.Body)
	}
	if w := authHTTPRequest(servers[0], "POST", "break-glass", fmt.Sprintf(`{"token":%q}`, token), authTestOrigin); w.Code != 204 {
		t.Fatalf("break-glass was account-limited: %d %s", w.Code, w.Body)
	}
	if _, err := pool.Exec(ctx, `UPDATE local_auth_attempts SET blocked_until=clock_timestamp()-interval '1 second' WHERE username=$1`, name); err != nil {
		t.Fatal(err)
	}
	if w := request(9, name, password); w.Code != 204 {
		t.Fatalf("cooldown did not recover: %d %s", w.Code, w.Body)
	}
}
