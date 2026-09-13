package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func authenticationBackend(t *testing.T) database.Backend {
	backend, _ := authenticationBackendFixture(t)
	return backend
}

func authenticationBackendFixture(t *testing.T) (database.Backend, database.Backend) {
	t.Helper()
	ctx := context.Background()
	if dsn := os.Getenv("PR02_DSN"); dsn != "" {
		options := mysql.Options{Engine: mysql.Engine(os.Getenv("PR02_ENGINE")), Environment: "test", DSN: dsn}
		admin, err := mysql.Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { admin.Close() })
		name := "pr02_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err = admin.Exec(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec(ctx, "DROP DATABASE `"+name+"`"); err != nil {
				t.Error(err)
			}
		})
		if _, err = admin.Exec(ctx, "GRANT ALL ON `"+name+"`.* TO 'ocservia_owner'@'%' WITH GRANT OPTION"); err != nil {
			t.Fatal(err)
		}
		config, err := driver.ParseDSN(dsn)
		if err != nil {
			t.Fatal("invalid test DSN")
		}
		config.DBName, config.User, config.Passwd = name, "ocservia_owner", "pr02-owner-test-only"
		options.DSN = config.FormatDSN()
		owner, err := mysql.Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { owner.Close() })
		if err = owner.Migrate(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if err = owner.MigrateTelemetryHistory(ctx); err != nil {
			t.Fatal(err)
		}
		if err = owner.GrantTestPrivileges(ctx); err != nil {
			t.Fatal(err)
		}
		config.User, config.Passwd = "ocservia_app", "pr02-runtime-test-only"
		options.DSN = config.FormatDSN()
		runtime, err := mysql.Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { runtime.Close() })
		return runtime, owner
	}
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("real PostgreSQL or PR02 database required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if ownerURL == "" {
		return postgres.WrapPool(pool), nil
	}
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	return postgres.WrapPool(pool), postgres.WrapPool(owner)
}

// This executes the actual Controller HTTP login, cookie authentication and
// logout handlers. It is one workflow, not acceptance of unrelated routes.
func TestAuthenticationBackendHTTPIntegration(t *testing.T) {
	backend := authenticationBackend(t)
	ctx := context.Background()
	service, err := auth.NewBackend(backend, auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	name := "http-" + uuid.NewString()
	password := "a long unique authentication test password"
	id, err := service.CreateLocalCredential(ctx, name, password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.CreateLocalCredential(ctx, name, password); !errors.Is(err, database.ErrUnique) {
		t.Fatalf("duplicate credential: %v", err)
	}
	server := NewBackend("127.0.0.1:0", backend, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, 15*time.Second, false, "", 35)
	server.auth = service
	server.EnableBrowserOrigin(authTestOrigin)
	login := func(password string) *httptest.ResponseRecorder {
		return authHTTPRequest(server, "POST", "login", `{"username":"`+name+`","password":"`+password+`"}`, authTestOrigin)
	}
	bad := login("incorrect")
	if bad.Code != http.StatusUnauthorized || len(bad.Result().Cookies()) != 0 {
		t.Fatalf("bad login: %d %s", bad.Code, bad.Body)
	}
	good := login(password)
	if good.Code != http.StatusNoContent || len(good.Result().Cookies()) != 1 {
		t.Fatalf("login: %d %s", good.Code, good.Body)
	}
	cookie := good.Result().Cookies()[0]
	principal, err := service.Authenticate(ctx, cookie)
	if err != nil || principal.IdentityID != id {
		t.Fatalf("session: %+v %v", principal, err)
	}
	// Stored infinite or extended-range expiry must never lengthen the finite
	// lifetime authenticated by the signed cookie.
	query := `UPDATE auth_sessions SET expires_at=$1 WHERE id=$2`
	var sessionID any = principal.SessionID
	if _, ok := backend.(*mysql.Backend); ok {
		query = `UPDATE auth_sessions SET expires_at=? WHERE id=?`
		sessionID = mysql.UUIDBytes(principal.SessionID)
	}
	for _, micros := range []int64{value.PositiveInfinity, value.EndTimestamp - 1} {
		if _, err := backend.Exec(ctx, query, value.Timestamp{Micros: micros, Valid: true}, sessionID); err != nil {
			t.Fatal(err)
		}
		bounded, err := service.Authenticate(ctx, cookie)
		if err != nil || !bounded.ExpiresAt.Truncate(time.Microsecond).Equal(principal.ExpiresAt) {
			t.Fatalf("logical session expiry %d: %+v %v", micros, bounded, err)
		}
	}
	logout := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	logout.Header.Set("Origin", authTestOrigin)
	logout.AddCookie(cookie)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, logout)
	if response.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", response.Code, response.Body)
	}
	if _, err = service.Authenticate(ctx, cookie); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("revoked cookie accepted: %v", err)
	}
	response = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, logout)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("replayed logout: %d", response.Code)
	}
	// Successful login cleared the durable single-flight lease in its session Tx.
	if response = login(password); response.Code != http.StatusNoContent {
		t.Fatalf("replacement login: %d %s", response.Code, response.Body)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = service.CreateLocalCredential(cancelled, "cancel-"+name, password); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled provisioning: %v", err)
	}
	workspace := uuid.Must(uuid.NewV7())
	if _, ok := backend.(*mysql.Backend); ok {
		_, err = backend.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'HTTP management',?,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, mysql.UUIDBytes(workspace), name)
	} else {
		_, err = backend.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'HTTP management',$2,now(),now())`, workspace, name)
		ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
		if ownerURL == "" {
			t.Fatal("owner URL required for isolated management fixture cleanup")
		}
		owner, openErr := pgxpool.New(ctx, ownerURL)
		if openErr != nil {
			t.Fatal(openErr)
		}
		t.Cleanup(func() {
			defer owner.Close()
			if _, err := owner.Exec(ctx, `DELETE FROM local_auth_bootstrap WHERE workspace_id=$1`, workspace); err != nil {
				t.Error(err)
			}
			if _, err := owner.Exec(ctx, `DELETE FROM role_bindings WHERE workspace_id=$1`, workspace); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	adminName := "admin-" + name
	adminID, err := service.BootstrapLocalAdmin(ctx, adminName, password, workspace, "approver-"+name, "a separate independent approver password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.BootstrapLocalAdmin(ctx, adminName, password, workspace, "approver-"+name, "a separate independent approver password"); !errors.Is(err, auth.ErrLocalInitialized) {
		t.Fatalf("repeat bootstrap: %v", err)
	}
	adminCookie, admin, err := service.AuthenticateLocal(ctx, adminName, password)
	if err != nil {
		t.Fatal(err)
	}
	if admin.IdentityID != adminID {
		t.Fatal("wrong bootstrap principal")
	}
	server.EnableAuthorization(service, rbac.NewBackend(backend), approvals.NewBackend(backend), nil)
	for _, node := range []string{"invalid", uuid.Must(uuid.NewV7()).String()} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/"+node, nil)
		r.AddCookie(adminCookie)
		w := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound || w.Header().Get("Content-Type") != "application/problem+json" {
			t.Fatalf("missing node authorization: %d %s", w.Code, w.Body)
		}
	}
	approvalHeader := ""
	call := func(path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Origin", authTestOrigin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Request-ID", uuid.NewString())
		r.Header.Set("X-Workspace-ID", workspace.String())
		if approvalHeader != "" {
			r.Header.Set("X-Approval-ID", approvalHeader)
		}
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(w, r)
		return w
	}
	memberName := "member-" + name
	created := call("/api/v1/local-users", `{"username":"`+memberName+`","password":"`+password+`"}`, adminCookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("HTTP create: %d %s", created.Code, created.Body)
	}
	var member struct {
		IdentityID uuid.UUID `json:"identity_id"`
	}
	if err = json.Unmarshal(created.Body.Bytes(), &member); err != nil {
		t.Fatal(err)
	}
	memberCookie, _, err := service.AuthenticateLocal(ctx, memberName, password)
	if err != nil {
		t.Fatal(err)
	}
	if denied := call("/api/v1/local-users", `{"username":"denied-`+name+`","password":"`+password+`"}`, memberCookie); denied.Code != http.StatusForbidden {
		t.Fatalf("member management allowed: %d %s", denied.Code, denied.Body)
	}
	newPassword := "a distinct replacement authentication password"
	changed := call("/api/v1/auth/change-password", `{"current_password":"`+password+`","new_password":"`+newPassword+`"}`, memberCookie)
	if changed.Code != http.StatusNoContent {
		t.Fatalf("HTTP password change: %d %s", changed.Code, changed.Body)
	}
	if _, err = service.Authenticate(ctx, memberCookie); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("old password session survived: %v", err)
	}
	memberCookie, _, err = service.AuthenticateLocal(ctx, memberName, newPassword)
	if err != nil {
		t.Fatal(err)
	}
	requested := call("/api/v1/approval-requests", `{"action":"local_user.reset-password","resource_type":"local_user","resource_id":"`+member.IdentityID.String()+`","reason":"restore local access","ttl_seconds":300}`, adminCookie)
	if requested.Code != http.StatusCreated {
		t.Fatalf("HTTP approval request: %d %s", requested.Code, requested.Body)
	}
	var approval approvals.Approval
	if err = json.Unmarshal(requested.Body.Bytes(), &approval); err != nil {
		t.Fatal(err)
	}
	decision := `{"reason":"independent review","expected_request_hash":"` + approval.RequestHash + `"}`
	path := "/api/v1/approval-requests/" + approval.ID.String() + ":approve"
	if self := call(path, decision, adminCookie); self.Code != http.StatusForbidden {
		t.Fatalf("HTTP self approval: %d %s", self.Code, self.Body)
	}
	approverCookie, _, err := service.AuthenticateLocal(ctx, "approver-"+name, "a separate independent approver password")
	if err != nil {
		t.Fatal(err)
	}
	if approved := call(path, decision, approverCookie); approved.Code != http.StatusOK {
		t.Fatalf("HTTP approval: %d %s", approved.Code, approved.Body)
	}
	approvalHeader = approval.ID.String()
	resetPassword := "an independently approved replacement password"
	reset := call("/api/v1/local-users/"+member.IdentityID.String()+":reset-password", `{"password":"`+resetPassword+`"}`, adminCookie)
	if reset.Code != http.StatusNoContent {
		t.Fatalf("HTTP approved reset: %d %s", reset.Code, reset.Body)
	}
	if _, err = service.Authenticate(ctx, memberCookie); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("reset left session active: %v", err)
	}
	if replay := call("/api/v1/local-users/"+member.IdentityID.String()+":reset-password", `{"password":"`+newPassword+`"}`, adminCookie); replay.Code != http.StatusConflict {
		t.Fatalf("HTTP approval replay: %d %s", replay.Code, replay.Body)
	}
	approvalHeader = ""
	memberCookie, _, err = service.AuthenticateLocal(ctx, memberName, resetPassword)
	if err != nil {
		t.Fatal(err)
	}
	disabled := call("/api/v1/local-users/"+member.IdentityID.String()+":disable", `{}`, adminCookie)
	if disabled.Code != http.StatusNoContent {
		t.Fatalf("HTTP disable: %d %s", disabled.Code, disabled.Body)
	}
	if _, err = service.Authenticate(ctx, memberCookie); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("disabled session survived: %v", err)
	}
	if protected := call("/api/v1/local-users/"+adminID.String()+":disable", `{}`, adminCookie); protected.Code != http.StatusConflict {
		t.Fatalf("last administrator disabled: %d %s", protected.Code, protected.Body)
	}
}
