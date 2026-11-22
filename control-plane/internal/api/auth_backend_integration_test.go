package api

import (
	"context"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/google/uuid"
)

func authenticationBackend(t *testing.T) database.Backend {
	backend, _ := authenticationBackendFixture(t)
	return backend
}

func authenticationBackendFixture(t *testing.T) (database.Backend, database.Backend) {
	return authenticationBackendFixtureWithIsolation(t, false)
}

func authenticationBackendFixtureWithIsolation(t *testing.T, isolated bool) (database.Backend, database.Backend) {
	t.Helper()
	if isolated {
		runtime, owner := testfixture.OpenIsolated(t, t, "r203_http")
		return postgres.WrapPool(runtime), postgres.WrapPool(owner)
	}
	return testfixture.OpenRuntime(t), nil
}

// This executes the actual Controller HTTP login, cookie authentication and
// logout handlers. It is one workflow, not acceptance of unrelated routes.
func TestAuthenticationBackendHTTPIntegration(t *testing.T) {
	backend, owner := authenticationBackendFixtureWithIsolation(t, true)
	ctx := context.Background()
	service, err := auth.NewBackend(backend, auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	testutil.NoError(t, err)
	name := "http-" + uuid.NewString()
	password := "a long unique authentication test password"
	id, err := service.CreateLocalCredential(ctx, name, password)
	testutil.NoError(t, err)
	if _, err = service.CreateLocalCredential(ctx, name, password); !errors.Is(err, database.ErrUnique) {
		t.Fatalf("duplicate credential: %v", err)
	}
	server := newTestServer(t, testHTTPConfig(false), backend, Modules{}, Authorization{Authentication: service})
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
	for _, micros := range []int64{value.PositiveInfinity, value.EndTimestamp - 1} {
		if _, err := backend.Exec(ctx, `UPDATE auth_sessions SET expires_at=$1 WHERE id=$2`, value.Timestamp{Micros: micros, Valid: true}, principal.SessionID); err != nil {
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
	if _, err := owner.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'HTTP management',$2,now(),now())`, workspace, name); err != nil {
		t.Fatal(err)
	}
	const adminName, bootstrapPassword = "admin", "nA6/X2kZ"
	adminID, err := service.BootstrapLocalAdmin(ctx, adminName, bootstrapPassword, workspace)
	testutil.NoError(t, err)
	if _, err = service.BootstrapLocalAdmin(ctx, adminName, bootstrapPassword, workspace); !errors.Is(err, auth.ErrLocalInitialized) {
		t.Fatalf("repeat bootstrap: %v", err)
	}
	adminCookie, admin, err := service.AuthenticateLocal(ctx, adminName, bootstrapPassword)
	testutil.NoError(t, err)
	if admin.IdentityID != adminID {
		t.Fatal("wrong bootstrap principal")
	}
	for _, node := range []string{"invalid", uuid.Must(uuid.NewV7()).String()} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/"+node, nil)
		r.AddCookie(adminCookie)
		w := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound || w.Header().Get("Content-Type") != "application/problem+json" {
			t.Fatalf("missing node authorization: %d %s", w.Code, w.Body)
		}
	}
	call := func(path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Origin", authTestOrigin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Request-ID", uuid.NewString())
		r.Header.Set("X-Workspace-ID", workspace.String())
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(w, r)
		return w
	}
	if retired := call("/api/v1/approval-requests", `{}`, adminCookie); retired.Code != http.StatusNotFound {
		t.Fatalf("retired approval API remained reachable: %d", retired.Code)
	}
	if retired := call("/api/v1/local-users", `{}`, adminCookie); retired.Code != http.StatusNotFound {
		t.Fatalf("retired local-users API remained reachable: %d", retired.Code)
	}
	changePassword := func(password string) *httptest.ResponseRecorder {
		return call("/api/v1/auth/change-password", `{"current_password":"`+bootstrapPassword+`","new_password":"`+password+`"}`, adminCookie)
	}
	if rejected := changePassword("f8V!q2x"); rejected.Code != http.StatusBadRequest {
		t.Fatalf("seven-character password: %d %s", rejected.Code, rejected.Body)
	}
	const newPassword = "f8V!q2xR"
	changed := changePassword(newPassword)
	if changed.Code != http.StatusNoContent {
		t.Fatalf("HTTP password change: %d %s", changed.Code, changed.Body)
	}
	if _, err = service.Authenticate(ctx, adminCookie); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("old password session survived: %v", err)
	}
	if _, _, err = service.AuthenticateLocal(ctx, adminName, newPassword); err != nil {
		t.Fatalf("re-login with changed password: %v", err)
	}
}
