package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
)

func TestLocalAdminPasswordResetIntegration(t *testing.T) {
	ctx := context.Background()
	runtime, owner := testfixture.OpenIsolated(t, t, "local_admin_reset")
	s, err := New(postgres.WrapPool(runtime), Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	testutil.NoError(t, err)
	if _, _, err := s.ResetLocalAdminPassword(ctx); !errors.Is(err, ErrLocalAdminUnavailable) {
		t.Fatalf("reset before bootstrap: %v", err)
	}
	const username, oldPassword = "first-admin", "original password for administrator reset integration"
	workspace := uuid.Must(uuid.NewV7())
	testutil.Exec(t, ctx, owner, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Admin reset','reset',now(),now())`, workspace)
	id, err := s.BootstrapLocalAdmin(ctx, username, oldPassword, workspace)
	testutil.NoError(t, err)
	first, _, err := s.AuthenticateLocal(ctx, username, oldPassword)
	testutil.NoError(t, err)
	second, _, err := s.AuthenticateLocal(ctx, username, oldPassword)
	testutil.NoError(t, err)

	testutil.Exec(t, ctx, owner, `UPDATE identities SET disabled_at=now() WHERE id=$1`, id)
	if _, _, err := s.ResetLocalAdminPassword(ctx); !errors.Is(err, ErrLocalAdminUnavailable) {
		t.Fatalf("reset disabled administrator: %v", err)
	}
	testutil.Exec(t, ctx, owner, `UPDATE identities SET disabled_at=NULL WHERE id=$1`, id)
	testutil.Exec(t, ctx, owner, `DELETE FROM workspace_admins WHERE identity_id=$1`, id)
	if _, _, err := s.ResetLocalAdminPassword(ctx); !errors.Is(err, ErrLocalAdminUnavailable) {
		t.Fatalf("reset unbound administrator: %v", err)
	}
	testutil.Exec(t, ctx, owner, `INSERT INTO workspace_admins(workspace_id,identity_id) VALUES($1,$2)`, workspace, id)

	// Model a login suspended after verifying the old credential and before
	// creating a session, along with an independently admitted password change.
	lease, err := s.reserveLocalAttempt(ctx, username)
	testutil.NoError(t, err)
	stale, err := s.localCredential(ctx, username)
	testutil.NoError(t, err)
	stale.attemptLease = lease
	_, err = s.reserveLocalAttempt(ctx, authstore.PasswordChangeAttemptKey(id))
	testutil.NoError(t, err)
	testutil.Exec(t, ctx, owner, `ALTER TABLE audit_events ADD CONSTRAINT test_admin_reset_audit_failure CHECK (action <> 'local_user.reset-password')`)
	if name, password, err := s.ResetLocalAdminPassword(ctx); err == nil || name != "" || password != "" {
		t.Fatal("failed audit transaction returned credentials or succeeded")
	}
	afterFailure, err := s.localCredential(ctx, username)
	testutil.NoError(t, err)
	if afterFailure.passwordHash != stale.passwordHash {
		t.Fatal("failed reset changed the password hash")
	}
	_, err = s.Authenticate(ctx, first)
	testutil.NoError(t, err)
	var attempts int
	testutil.NoError(t, owner.QueryRow(ctx, `SELECT count(*) FROM local_auth_attempts WHERE username IN ($1,$2)`, username, authstore.PasswordChangeAttemptKey(id)).Scan(&attempts))
	if attempts != 2 {
		t.Fatal("failed reset removed attempt state")
	}
	testutil.Exec(t, ctx, owner, `ALTER TABLE audit_events DROP CONSTRAINT test_admin_reset_audit_failure`)

	name, password, err := s.ResetLocalAdminPassword(ctx)
	testutil.NoError(t, err)
	if name != username || len(password) != 32 {
		t.Fatal("reset did not return the bootstrapped username and a generated credential")
	}
	testutil.NoError(t, owner.QueryRow(ctx, `SELECT count(*) FROM local_auth_attempts WHERE username IN ($1,$2)`, username, authstore.PasswordChangeAttemptKey(id)).Scan(&attempts))
	if attempts != 0 {
		t.Fatal("reset left attempt state")
	}
	if _, _, err := s.createSession(ctx, LocalIssuer, username, &stale); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("in-flight login with the old password issued a session: %v", err)
	}
	for _, cookie := range []*http.Cookie{first, second} {
		if _, err := s.Authenticate(ctx, cookie); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("old cookie survived reset: %v", err)
		}
	}
	if _, _, err := s.AuthenticateLocal(ctx, username, oldPassword); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old password survived reset: %v", err)
	}
	_, principal, err := s.AuthenticateLocal(ctx, username, password)
	testutil.NoError(t, err)
	if principal.IdentityID != id {
		t.Fatal("reset changed the administrator identity")
	}
	var auditAndBinding bool
	testutil.NoError(t, owner.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND resource_id=$2
		 AND action='local_user.reset-password' AND result='succeeded' AND auth_version=1)=1
		AND EXISTS (SELECT 1 FROM local_auth_bootstrap b JOIN workspace_admins a USING(workspace_id,identity_id)
		 WHERE b.workspace_id=$1 AND b.identity_id=$2)`, workspace, id).Scan(&auditAndBinding))
	if !auditAndBinding {
		t.Fatal("reset did not preserve the bootstrap binding and append its audit event")
	}
}

func TestLocalAuthenticationIntegration(t *testing.T) {
	ctx := context.Background()
	pool := testfixture.OpenRuntimePool(t)
	s, err := New(postgres.WrapPool(pool), Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	testutil.NoError(t, err)
	username := "alice-" + uuid.NewString()
	if _, err := s.CreateLocalCredential(ctx, username, "x"); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("weak provisioning: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM identities WHERE subject=$1)+(SELECT count(*) FROM local_credentials WHERE username=$1)`, username).Scan(&count); err != nil || count != 0 {
		t.Fatalf("weak provisioning wrote rows: %d %v", count, err)
	}
	password := "a local test password"
	id, err := s.CreateLocalCredential(ctx, " "+strings.ToUpper(username)+" ", password)
	testutil.NoError(t, err)
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
	realNow := s.now
	s.now = func() time.Time { return principal.ExpiresAt.Add(time.Second) }
	if _, err := s.Authenticate(ctx, cookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired local cookie accepted: %v", err)
	}
	s.now = realNow
	if _, err := pool.Exec(ctx, `UPDATE auth_sessions SET created_at=now()-interval '1 hour',expires_at=now()-interval '1 second' WHERE id=$1`, principal.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, cookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired database session accepted: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE auth_sessions SET expires_at=$2 WHERE id=$1`, principal.SessionID, principal.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if err := s.Logout(ctx, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, cookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("logged-out session accepted: %v", err)
	}
	cookie, _, err = s.AuthenticateLocal(ctx, username, password)
	testutil.NoError(t, err)
	if _, err := pool.Exec(ctx, `UPDATE identities SET disabled_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuthenticateLocal(ctx, username, password); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("disabled local login accepted: %v", err)
	}
	if _, err := s.Authenticate(ctx, cookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("disabled identity session accepted: %v", err)
	}
	if _, _, err := s.createSession(ctx, LocalIssuer, username, &credential); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("disable after verification accepted: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identities SET disabled_at=NULL WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE local_credentials SET password_hash=$2 WHERE identity_id=$1`, id, dummyPasswordHash); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.createSession(ctx, LocalIssuer, username, &credential); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("changed credential accepted: %v", err)
	}
	var sessions, admins int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM auth_sessions WHERE identity_id=$1),(SELECT count(*) FROM workspace_admins WHERE identity_id=$1)`, id).Scan(&sessions, &admins); err != nil || sessions != 2 || admins != 0 {
		t.Fatalf("failed logins created sessions or provisioning granted workspace_admins: %d/%d, %v", sessions, admins, err)
	}
	for _, legacy := range []string{"x", "password", "12345678901234567890"} {
		if _, err := pool.Exec(ctx, `UPDATE local_credentials SET password_hash=$2 WHERE identity_id=$1`, id, legacyPasswordHash(legacy)); err != nil {
			t.Fatal(err)
		}
		cookie, principal, err := s.AuthenticateLocal(ctx, username, legacy)
		if err != nil || cookie == nil || principal.IdentityID != id {
			t.Fatalf("historical login rejected: %v", err)
		}
		if _, err := s.Authenticate(ctx, cookie); err != nil {
			t.Fatalf("historical login session rejected: %v", err)
		}
	}
}
