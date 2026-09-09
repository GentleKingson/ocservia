package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
)

func captureAuthLogs(s *Server) *bytes.Buffer {
	b := new(bytes.Buffer)
	s.logger = slog.New(slog.NewJSONHandler(b, nil))
	return b
}

func authLogRecords(t *testing.T, b *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("invalid structured log: %v", err)
		}
		if record["event"] != nil {
			records = append(records, record)
		}
	}
	return records
}

func assertAuthLog(t *testing.T, b *bytes.Buffer, method, outcome, reason string) map[string]any {
	t.Helper()
	records := authLogRecords(t, b)
	if len(records) == 0 {
		t.Fatal("missing security event")
	}
	r := records[len(records)-1]
	if r["event"] != "auth.result" || r["auth_method"] != method || r["outcome"] != outcome || r["reason_code"] != reason || r["request_id"] == "" || r["source_ip"] == "" {
		t.Fatalf("unexpected authentication event: %v", r)
	}
	if outcome != "succeeded" && r["identity_id"] != nil {
		t.Fatal("untrusted identity logged")
	}
	return r
}

func assertNoAuthSecrets(t *testing.T, b *bytes.Buffer, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(b.String(), secret) {
			t.Fatal("sensitive bait appeared in log")
		}
	}
}

func TestAuthLogKindsAndBoundedFields(t *testing.T) {
	s := newAuthHTTPServer(t, nil, true, "")
	b := captureAuthLogs(s)
	id := uuid.New()
	r := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, "request-r6"))
	for kind := authLogKind(0); kind < authLogKinds; kind++ {
		s.logAuth(r, kind, id, "")
		d := authLogDefinitions[kind]
		record := assertAuthLog(t, b, d.method, d.outcome, d.reason)
		if record["request_id"] != "request-r6" {
			t.Fatal("request correlation missing")
		}
		if d.outcome == "succeeded" && record["identity_id"] != id.String() {
			t.Fatal("trusted identity missing")
		}
	}
	if len(authLogRecords(t, b)) != int(authLogKinds) {
		t.Fatal("duplicate final event")
	}
	for _, username := range []string{"bad\r\nforged-event", strings.Repeat("x", 8000)} {
		b.Reset()
		body, _ := json.Marshal(map[string]string{"username": username, "password": "password-bait-r6"})
		r := httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewReader(body))
		r.Header.Set("Origin", authTestOrigin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Request-ID", "injected\r\n"+strings.Repeat("z", 500))
		r.Header.Set("Cookie", "session-bait-r6")
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("status=%d", w.Code)
		}
		record := assertAuthLog(t, b, "local", "rejected", "credentials_rejected")
		if record["account_ref"] != "invalid" || record["request_id"] != w.Header().Get("X-Request-ID") || b.Len() > 700 || bytes.Count(b.Bytes(), []byte("\n")) != 1 {
			t.Fatal("unbounded or injected log")
		}
		assertNoAuthSecrets(t, b, username, "password-bait-r6", "session-bait-r6", "injected")
	}
}

func TestAuthLogConcurrentAggregation(t *testing.T) {
	var state authLogState
	var emitted atomic.Int64
	var workers sync.WaitGroup
	now := time.Unix(1000, 0)
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 100; j++ {
				ok, _ := state.admit(localSourceLimited, now)
				if ok {
					emitted.Add(1)
				}
			}
		}()
	}
	workers.Wait()
	_, counts := state.admit(oidcStarted, now.Add(time.Minute))
	if emitted.Load() != authLogSamples || counts[localSourceLimited] != 1600-authLogSamples {
		t.Fatal("concurrent logging exceeded sample budget or lost counts")
	}
}

func TestAuthLogSourceAndAdmission(t *testing.T) {
	for _, peer := range []string{"192.0.2.1:1234", "10.0.0.2:1234"} {
		s := newAuthHTTPServer(t, nil, true, "")
		s.ConfigureAuthProxies([]netip.Prefix{netip.MustParsePrefix("10.0.0.2/32")})
		b := captureAuthLogs(s)
		for i := 0; i < 100; i++ {
			r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"username":"","password":""}`))
			r.Header.Set("Origin", authTestOrigin)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Forwarded-For", "203.0.113.9")
			r.Header.Set("X-Ocservia-Client-IP", "198.51.100.1")
			r.RemoteAddr = peer
			w := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, r)
			want := 401
			if i >= 5 {
				want = 429
			}
			if w.Code != want {
				t.Fatal("logging changed authentication budget")
			}
		}
		record := assertAuthLog(t, b, "local", "rejected", "source_limited")
		want := "192.0.2.1"
		if strings.HasPrefix(peer, "10.") {
			want = "198.51.100.1"
		}
		if record["source_ip"] != want || len(authLogRecords(t, b)) != 15 || bytes.Count(b.Bytes(), []byte("\n")) != 15 {
			t.Fatal("source trust or log budget bypass")
		}
		s.authLogs.until = time.Now().Add(-time.Second)
		authHTTPRequest(s, "POST", "login", `{}`, authTestOrigin)
		records := authLogRecords(t, b)
		if len(records) != 17 || records[15]["event"] != "auth.summary" || records[15]["suppressed_count"] != float64(85) {
			t.Fatal("missing rejection aggregate")
		}
	}
	for _, test := range []struct {
		kind   authLogKind
		budget *authAdmission
	}{
		{localGlobalLimited, newAuthAdmission(5, 1, 1)},
		{localConcurrencyLimited, newAuthAdmission(5, 100, 1)},
		{localSourceCapacityLimited, newAuthAdmission(5, 100, 1)},
	} {
		s := newAuthHTTPServer(t, nil, true, "")
		b := captureAuthLogs(s)
		s.localLoginBudget = test.budget
		switch test.kind {
		case localGlobalLimited:
			test.budget.global = authWindow{count: 1, until: time.Now().Add(time.Minute)}
		case localConcurrencyLimited:
			test.budget.active <- struct{}{}
		case localSourceCapacityLimited:
			for i := 0; i < authSourceCapacity; i++ {
				test.budget.sources[netip.MustParseAddr(fmt.Sprintf("2001:db8::%x", i+1))] = authWindow{until: time.Now().Add(time.Minute)}
			}
		}
		w := authHTTPRequest(s, "POST", "login", `{}`, authTestOrigin)
		if w.Code != 429 {
			t.Fatal("budget bypass")
		}
		assertAuthLog(t, b, "local", "rejected", authLogDefinitions[test.kind].reason)
	}
}

type brokenAuthHandler struct{ panic bool }

func (h brokenAuthHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h brokenAuthHandler) Handle(context.Context, slog.Record) error {
	if h.panic {
		panic("logger failure")
	}
	return errors.New("logger failure")
}
func (h brokenAuthHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h brokenAuthHandler) WithGroup(string) slog.Handler      { return h }

func TestAuthLogFailureAndInfrastructure(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:db-connection-bait@127.0.0.1:1/test")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	s := newAuthHTTPServer(t, pool, true, "")
	b := captureAuthLogs(s)
	w := authHTTPRequest(s, "POST", "login", `{"username":"valid-name","password":"password-bait-r6"}`, authTestOrigin)
	if w.Code != 503 || len(w.Result().Cookies()) != 0 {
		t.Fatal("infrastructure failure authorized")
	}
	assertAuthLog(t, b, "local", "unavailable", "infrastructure_failure")
	assertNoAuthSecrets(t, b, "db-connection-bait", "password-bait-r6", "valid-name")
	for _, panics := range []bool{false, true} {
		s := newAuthHTTPServer(t, nil, true, "https://idp.example.test")
		s.logger = slog.New(brokenAuthHandler{panic: panics})
		for i := 0; i < 20; i++ {
			w := authHTTPRequest(s, "POST", "login", `{"username":"","password":""}`, authTestOrigin)
			want := 401
			if i >= 5 {
				want = 429
			}
			if w.Code != want || len(w.Result().Cookies()) != 0 {
				t.Fatal("logger failure changed rejection")
			}
		}
		if w := authHTTPRequest(s, "GET", "callback?code=code-bait-r6", "", ""); w.Code != http.StatusUnauthorized {
			t.Fatal("logger failure changed callback")
		}
	}
}

func TestAuthLogOIDCStartFailureRedacted(t *testing.T) {
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "client-secret-bait-r6 id-token-bait-r6", 503)
	}))
	defer idp.Close()
	s := newAuthHTTPServer(t, nil, false, idp.URL)
	b := captureAuthLogs(s)
	r := httptest.NewRequest("GET", "/api/v1/auth/login", nil)
	r = r.WithContext(context.WithValue(r.Context(), oauth2.HTTPClient, idp.Client()))
	w := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(w, r)
	if w.Code != 503 || len(w.Result().Cookies()) != 0 {
		t.Fatal("failed discovery issued login")
	}
	assertAuthLog(t, b, "oidc", "unavailable", "start_failed")
	assertNoAuthSecrets(t, b, "client-secret-bait-r6", "id-token-bait-r6", idp.URL)
}

func TestAuthLogSessionFailureIntegration(t *testing.T) {
	url, ownerURL := os.Getenv("OCSERV_TEST_DATABASE_URL"), os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if url == "" || ownerURL == "" {
		t.Skip("isolated runtime and owner database URLs required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	s := newAuthHTTPServer(t, pool, true, "")
	b := captureAuthLogs(s)
	name, password := "r6-"+uuid.NewString(), "r6-session-password-bait"
	id, err := s.auth.CreateLocalCredential(ctx, name, password)
	if err != nil {
		t.Fatal(err)
	}
	constraint := "r6_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := owner.Exec(ctx, `ALTER TABLE auth_sessions ADD CONSTRAINT `+constraint+` CHECK (identity_id <> '`+id.String()+`'::uuid) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = owner.Exec(ctx, `ALTER TABLE auth_sessions DROP CONSTRAINT IF EXISTS `+constraint) }()
	body, _ := json.Marshal(map[string]string{"username": name, "password": password})
	w := authHTTPRequest(s, "POST", "login", string(body), authTestOrigin)
	if w.Code != 503 || len(w.Result().Cookies()) != 0 {
		t.Fatal("failed session insert issued success")
	}
	assertAuthLog(t, b, "local", "unavailable", "infrastructure_failure")
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE identity_id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("session failure did not roll back")
	}
	if _, err := owner.Exec(ctx, `ALTER TABLE auth_sessions DROP CONSTRAINT `+constraint); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := pool.QueryRow(ctx, `SELECT password_hash FROM local_credentials WHERE identity_id=$1`, id).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	assertNoAuthSecrets(t, b, name, password, hash, id.String())
	for _, panics := range []bool{false, true} {
		s.logger = slog.New(brokenAuthHandler{panic: panics})
		w = authHTTPRequest(s, "POST", "login", string(body), authTestOrigin)
		if w.Code != 204 || len(w.Result().Cookies()) != 1 {
			t.Fatal("log failure changed successful login")
		}
		actor, err := s.auth.Authenticate(ctx, w.Result().Cookies()[0])
		if err != nil || actor.IdentityID != id || actor.BreakGlass {
			t.Fatal("log failure changed principal")
		}
	}
	s.authLogs.counts[localSucceeded] = authLogSamples
	b = captureAuthLogs(s)
	w = authHTTPRequest(s, "POST", "login", string(body), authTestOrigin)
	if w.Code != 204 || b.Len() != 0 {
		t.Fatal("aggregation changed success or exceeded log budget")
	}
	actor, err := s.auth.Authenticate(ctx, w.Result().Cookies()[0])
	if err != nil || actor.IdentityID != id {
		t.Fatal("aggregation changed session")
	}
	if _, err := pool.Exec(ctx, `UPDATE identities SET disabled_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	w = authHTTPRequest(s, "POST", "login", string(body), authTestOrigin)
	if w.Code != 401 || len(w.Result().Cookies()) != 0 || !strings.Contains(w.Body.String(), "Invalid username or password") {
		t.Fatal("disabled account response disclosed identity state")
	}
	assertAuthLog(t, b, "local", "rejected", "credentials_rejected")
}
