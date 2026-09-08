package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLocalAuthenticationIntegration(t *testing.T) {
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
	s, err := New(ctx, pool, Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	username := "alice-" + uuid.NewString()
	password := "a local test password"
	id, err := s.CreateLocalCredential(ctx, " "+strings.ToUpper(username)+" ", password)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := s.localCredential(ctx, username)
	if err != nil || credential.identityID != id || credential.disabled || credential.passwordHash == password {
		t.Fatalf("read credential: %+v, %v", credential, err)
	}
	var timestamps bool
	if err := pool.QueryRow(ctx, `SELECT created_at=updated_at AND created_at=password_changed_at FROM local_credentials WHERE identity_id=$1`, id).Scan(&timestamps); err != nil || !timestamps {
		t.Fatalf("credential timestamps: %v, %v", timestamps, err)
	}
	if _, err := s.CreateLocalCredential(ctx, strings.ToUpper(username), "replacement password"); err == nil {
		t.Fatal("duplicate username replaced a credential")
	}

	// Same subject and profile claims are not an account-linking mechanism.
	if _, err := pool.Exec(ctx, `UPDATE identities SET email='same@example.test',display_name='Same Name' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	oidcCookie, oidcPrincipal, err := s.createSession(ctx, "https://idp.example.com", username, "same@example.test", "Same Name", false, nil)
	if err != nil || oidcPrincipal.IdentityID == id {
		t.Fatalf("OIDC identity isolation: %+v, %v", oidcPrincipal, err)
	}
	for _, attempt := range []struct{ username, password string }{
		{username, "wrong"}, {"missing-" + username, password}, {username, strings.Repeat("p", maxPasswordBytes+1)},
	} {
		if cookie, principal, err := s.AuthenticateLocal(ctx, attempt.username, attempt.password); !errors.Is(err, ErrUnauthenticated) || cookie != nil || principal.IdentityID != uuid.Nil {
			t.Fatalf("invalid login: %+v, %v", principal, err)
		}
	}
	cookie, principal, err := s.AuthenticateLocal(ctx, strings.ToUpper(username), password)
	if err != nil || principal.IdentityID != id || principal.Issuer != LocalIssuer || principal.Subject != username || principal.BreakGlass {
		t.Fatalf("local login: %+v, %v", principal, err)
	}
	if cookie.Name != SessionCookieName || cookie.Path != "/" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("nonstandard session cookie: %+v", cookie)
	}
	var standard bool
	if err := pool.QueryRow(ctx, `SELECT identity_id=$2 AND NOT break_glass AND revoked_at IS NULL AND expires_at>created_at FROM auth_sessions WHERE id=$1`, principal.SessionID, id).Scan(&standard); err != nil || !standard {
		t.Fatalf("standard auth_session: %v, %v", standard, err)
	}
	if got, err := s.Authenticate(ctx, cookie); err != nil || got.IdentityID != id {
		t.Fatalf("authenticate local session: %+v, %v", got, err)
	}
	var profiles int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identities WHERE subject=$1 AND email='same@example.test' AND display_name='Same Name'`, username).Scan(&profiles); err != nil || profiles != 2 {
		t.Fatalf("profiles changed/merged: %d, %v", profiles, err)
	}
	if err := s.Logout(ctx, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, cookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("logged-out session accepted: %v", err)
	}
	cookie, _, err = s.AuthenticateLocal(ctx, username, password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identities SET disabled_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuthenticateLocal(ctx, username, password); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("disabled local login accepted: %v", err)
	}
	if _, err := s.Authenticate(ctx, cookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("disabled identity session accepted: %v", err)
	}
	if _, _, err := s.createSession(ctx, LocalIssuer, username, "", "", false, &credential); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("disable after verification accepted: %v", err)
	}
	if got, err := s.Authenticate(ctx, oidcCookie); err != nil || got.IdentityID != oidcPrincipal.IdentityID {
		t.Fatalf("local disable affected OIDC: %+v, %v", got, err)
	}
	if err := s.Logout(ctx, oidcPrincipal); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, oidcCookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("OIDC logout failed: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identities SET disabled_at=NULL WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE local_credentials SET password_hash=$2 WHERE identity_id=$1`, id, dummyPasswordHash); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.createSession(ctx, LocalIssuer, username, "", "", false, &credential); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("changed credential accepted: %v", err)
	}
	var sessions, bindings int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM auth_sessions WHERE identity_id=$1),(SELECT count(*) FROM role_bindings WHERE identity_id=$1)`, id).Scan(&sessions, &bindings); err != nil || sessions != 2 || bindings != 0 {
		t.Fatalf("failed logins created sessions or provisioning granted roles: %d/%d, %v", sessions, bindings, err)
	}
}
