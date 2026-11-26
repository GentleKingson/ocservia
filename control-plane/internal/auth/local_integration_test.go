//go:build integration

package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
)

func TestLocalAuthenticationAndResetIntegration(t *testing.T) {
	ctx := context.Background()
	runtime, owner := testfixture.OpenIsolated(t, t, "local_auth")
	s, err := New(postgres.WrapPool(runtime), Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	testutil.NoError(t, err)

	workspace := uuid.Must(uuid.NewV7())
	testutil.Exec(t, ctx, owner, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Admin reset','reset',now(),now())`, workspace)
	const username, oldPassword = "first-admin", "original password for administrator reset integration"
	id, err := s.BootstrapLocalAdmin(ctx, username, oldPassword, workspace)
	testutil.NoError(t, err)

	cookie, principal, err := s.AuthenticateLocal(ctx, username, oldPassword)
	testutil.NoError(t, err)
	if principal.IdentityID != id || cookie.Name != SessionCookieName || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected local login result: %+v %+v", principal, cookie)
	}
	if _, err := s.Authenticate(ctx, cookie); err != nil {
		t.Fatalf("session authenticate failed: %v", err)
	}
	if _, _, err := s.AuthenticateLocal(ctx, username, "wrong"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("wrong password accepted: %v", err)
	}

	name, newPassword, err := s.ResetLocalAdminPassword(ctx)
	testutil.NoError(t, err)
	if name != username || len(newPassword) != 32 {
		t.Fatalf("unexpected reset output: %q %d", name, len(newPassword))
	}
	if _, err := s.Authenticate(ctx, cookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old cookie survived reset: %v", err)
	}
	if _, _, err := s.AuthenticateLocal(ctx, username, oldPassword); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old password survived reset: %v", err)
	}
	if _, p2, err := s.AuthenticateLocal(ctx, username, newPassword); err != nil || p2.IdentityID != id {
		t.Fatalf("login with reset password failed: %v", err)
	}
}
