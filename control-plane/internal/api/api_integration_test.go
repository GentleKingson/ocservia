//go:build integration

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
)

func TestAuthenticationBackendHTTPIntegration(t *testing.T) {
	runtime, owner := testfixture.OpenIsolated(t, t, "api_http")
	backend := postgres.WrapPool(runtime)
	ctx := context.Background()
	service, err := auth.New(backend, auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	testutil.NoError(t, err)

	name := "http-" + uuid.NewString()
	password := "a long unique authentication test password"
	id, err := service.CreateLocalCredential(ctx, name, password)
	testutil.NoError(t, err)
	if _, err = service.CreateLocalCredential(ctx, name, password); !errors.Is(err, database.ErrUnique) {
		t.Fatalf("duplicate credential: %v", err)
	}

	server := newTestServer(t, testHTTPConfig(false), backend, Modules{}, Authorization{Authentication: service})
	login := func(pw string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"`+name+`","password":"`+pw+`"}`))
		r.Header.Set("Origin", authTestOrigin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(w, r)
		return w
	}
	if bad := login("incorrect"); bad.Code != http.StatusUnauthorized || len(bad.Result().Cookies()) != 0 {
		t.Fatalf("bad login: %d", bad.Code)
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

	logout := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logout.Header.Set("Origin", authTestOrigin)
	logout.AddCookie(cookie)
	w := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(w, logout)
	if w.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", w.Code, w.Body)
	}
	if _, err = service.Authenticate(ctx, cookie); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("revoked cookie accepted: %v", err)
	}

	workspace := uuid.Must(uuid.NewV7())
	if _, err := owner.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'HTTP management',$2,now(),now())`, workspace, name); err != nil {
		t.Fatal(err)
	}
	const adminName, bootstrapPassword = "admin", "nA6/X2kZ"
	if _, err := service.BootstrapLocalAdmin(ctx, adminName, bootstrapPassword, workspace); err != nil {
		t.Fatal(err)
	}
	adminCookie, _, err := service.AuthenticateLocal(ctx, adminName, bootstrapPassword)
	testutil.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/"+uuid.Must(uuid.NewV7()).String(), nil)
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing node status = %d", rec.Code)
	}

	change := httptest.NewRequest(http.MethodPost, "/api/v1/auth/change-password", strings.NewReader(`{"current_password":"`+bootstrapPassword+`","new_password":"f8V!q2xR"}`))
	change.Header.Set("Origin", authTestOrigin)
	change.Header.Set("Content-Type", "application/json")
	change.Header.Set("X-Workspace-ID", workspace.String())
	change.AddCookie(adminCookie)
	rec = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rec, change)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("password change: %d %s", rec.Code, rec.Body)
	}
	if _, err = service.Authenticate(ctx, adminCookie); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("old password session survived: %v", err)
	}
}
