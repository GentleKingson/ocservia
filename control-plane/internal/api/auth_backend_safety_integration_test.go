package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/identityprofile"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/google/uuid"
)

func authSafetyExec(t *testing.T, b database.Backend, query string, args ...any) {
	t.Helper()
	if _, err := b.Exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func authSafetyWorkspace(t *testing.T, owner database.Backend) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now, err := value.FromTime(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	authSafetyExec(t, owner, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Authentication safety test',$2,$3,$4)`, id, "auth-safety-"+id.String(), now, now)
	t.Cleanup(func() {
		authSafetyExec(t, owner, `DELETE FROM local_auth_bootstrap WHERE workspace_id=$1`, id)
		authSafetyExec(t, owner, `DELETE FROM role_bindings WHERE workspace_id=$1`, id)
	})
	return id
}

func authSafetyAuditFailure(t *testing.T, owner database.Backend, workspace uuid.UUID, action string) func() {
	t.Helper()
	authSafetyExec(t, owner, `ALTER TABLE audit_events ADD CONSTRAINT test_auth_audit_failure CHECK(workspace_id <> '`+workspace.String()+`' OR action <> '`+action+`')`)
	removed := false
	drop := func() {
		if removed {
			return
		}
		authSafetyExec(t, owner, `ALTER TABLE audit_events DROP CONSTRAINT test_auth_audit_failure`)
		removed = true
	}
	t.Cleanup(drop)
	return drop
}

func TestAuthenticationBackendSafetyIntegration(t *testing.T) {
	b, owner := authenticationBackendFixture(t)
	if owner == nil {
		t.Fatal("owner URL required for isolated authentication fixtures")
	}
	ctx := context.Background()
	var user string
	if err := b.QueryRow(ctx, `SELECT current_user`).Scan(&user); err != nil || !strings.HasPrefix(user, "ocservia_app") {
		t.Fatalf("restricted runtime required: %q %v", user, err)
	}
	for _, table := range []string{"audit_events", "audit_checkpoints"} {
		for _, query := range []string{"UPDATE " + table + " SET id=id", "DELETE FROM " + table, "TRUNCATE TABLE " + table} {
			if _, err := b.Exec(ctx, query); !errors.Is(err, database.ErrPermission) {
				t.Fatalf("runtime audit mutation not denied: %s: %v", query, err)
			}
		}
	}

	t.Run("capacity", func(t *testing.T) {
		var count, baseline int
		if err := b.QueryRow(ctx, `SELECT count(*) FROM local_auth_attempts`).Scan(&baseline); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			authSafetyExec(t, b, `DELETE FROM local_auth_attempts WHERE username LIKE 'auth-capacity-%'`)
		})
		results := make(chan error, 16)
		for i := range cap(results) {
			go func() {
				results <- database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
					store, err := authstore.From(tx)
					if err != nil {
						return err
					}
					admitted, err := store.ReserveAttempt(ctx, fmt.Sprintf("auth-capacity-%d", i), uuid.New(), baseline+8, 15*time.Minute, 30*time.Second)
					if err == nil && !admitted {
						return errors.New("distinct account unexpectedly refused")
					}
					return err
				})
			}()
		}
		succeeded := 0
		for range cap(results) {
			if err := <-results; err == nil {
				succeeded++
			} else if !errors.Is(err, authstore.ErrAttemptCapacity) {
				t.Error(err)
			}
		}
		if err := b.QueryRow(ctx, `SELECT count(*) FROM local_auth_attempts`).Scan(&count); err != nil || count != baseline+8 || succeeded != 8 {
			t.Fatalf("capacity overshoot: count=%d successes=%d err=%v", count, succeeded, err)
		}
	})

	s, err := auth.NewBackend(b, auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	wid := authSafetyWorkspace(t, owner)
	other := authSafetyWorkspace(t, owner)
	name := "auth-safety-" + uuid.NewString()
	const password = "auth-safety administrator credential"
	const approverPassword = "auth-safety independent approver credential"
	bootstrap := func() error {
		_, err := s.BootstrapLocalAdmin(ctx, name, password, wid, name+"-approver", approverPassword)
		return err
	}
	drop := authSafetyAuditFailure(t, owner, wid, "local_user.bootstrap")
	if err := bootstrap(); err == nil {
		t.Fatal("partial initialization committed after audit failure")
	}
	for _, username := range []string{name, name + "-approver"} {
		if _, err := authstore.ReadCredential(ctx, b, username); !errors.Is(err, database.ErrNotFound) {
			t.Fatalf("failed bootstrap retained credential: %v", err)
		}
	}
	var count int
	if err := b.QueryRow(ctx, `SELECT count(*) FROM local_auth_bootstrap`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("bootstrap must start with absent singleton: %d %v", count, err)
	}
	t.Run("absent-singleton-lock", func(t *testing.T) {
		holder, err := b.Begin(ctx, database.ReadCommitted)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(ctx)
		store, err := authstore.From(holder)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.LockManagement(ctx); err != nil {
			t.Fatal(err)
		}
		wait, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		err = database.Within(wait, b, database.ReadCommitted, func(tx database.Tx) error {
			store, err := authstore.From(tx)
			if err != nil {
				return err
			}
			return store.LockManagement(wait)
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("management lock did not serialize the absent singleton: %v", err)
		}
	})
	drop()
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- bootstrap() }()
	}
	succeeded := 0
	for range 2 {
		if err := <-results; err == nil {
			succeeded++
		} else if !errors.Is(err, auth.ErrLocalInitialized) {
			t.Error(err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("concurrent bootstrap successes: %d", succeeded)
	}
	m := audit.NewBackendManager(b, bytes.Repeat([]byte{7}, 32))
	verified, err := m.Verify(ctx, wid)
	if err != nil || !verified.Valid || verified.Events != 1 {
		t.Fatalf("bootstrap audit rollback: %+v %v", verified, err)
	}
	_, admin, err := s.AuthenticateLocal(ctx, name, password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authstore.ReadCredential(ctx, b, name+"-approver"); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("bootstrap provisioned a second identity: %v", err)
	}
	if _, err := s.CompleteLocalBootstrap(ctx, name, password, wid, name+"-extra", approverPassword); !errors.Is(err, auth.ErrLocalInitialized) {
		t.Fatalf("completed bootstrap reopened: %v", err)
	}
	mutate := func(actor auth.Principal, action string, id, workspace, approval uuid.UUID) error {
		_, err := s.MutateLocalUser(ctx, auth.LocalUserMutation{Action: action, IdentityID: id, ActorID: actor.IdentityID, SessionID: actor.SessionID, WorkspaceID: workspace, ApprovalID: approval, Password: "auth-safety replacement credential", RequestID: uuid.NewString()})
		return err
	}
	for _, id := range []uuid.UUID{admin.IdentityID} {
		if err := mutate(admin, "disable", id, wid, uuid.Nil); !errors.Is(err, auth.ErrLocalProtected) {
			t.Fatalf("last administrator not protected: %v", err)
		}
	}
	roles := rbac.NewBackend(b)
	if err := roles.Authorize(ctx, admin.IdentityID, "local_user.manage", rbac.Resource{WorkspaceID: other, Type: "workspace"}, false); !errors.Is(err, rbac.ErrForbidden) {
		t.Fatalf("cross-workspace authorization: %v", err)
	}
	if err := mutate(admin, "disable", admin.IdentityID, other, uuid.Nil); !errors.Is(err, auth.ErrLocalInvalid) {
		t.Fatalf("management workspace changed: %v", err)
	}

	memberID, err := s.MutateLocalUser(ctx, auth.LocalUserMutation{Action: "create", Username: name + "-member", Password: password, ActorID: admin.IdentityID, SessionID: admin.SessionID, WorkspaceID: wid, RequestID: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	memberCookie, _, err := s.AuthenticateLocal(ctx, name+"-member", password)
	if err != nil {
		t.Fatal(err)
	}
	approvalID := uuid.Nil
	before, err := authstore.ReadCredential(ctx, b, name+"-member")
	if err != nil {
		t.Fatal(err)
	}
	drop = authSafetyAuditFailure(t, owner, wid, "local_user.reset-password")
	if err := mutate(admin, "reset-password", memberID, wid, approvalID); err == nil {
		t.Fatal("reset committed after audit failure")
	}
	after, err := authstore.ReadCredential(ctx, b, name+"-member")
	if err != nil || before.Hash != after.Hash {
		t.Fatalf("password rollback: %v", err)
	}
	if _, err := s.Authenticate(ctx, memberCookie); err != nil {
		t.Fatalf("revocation escaped rollback: %v", err)
	}
	drop()
	if err := mutate(admin, "reset-password", memberID, wid, approvalID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, memberCookie); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("reset retained old session: %v", err)
	}
	if _, err := roles.CreateBinding(ctx, rbac.BindingRequest{IdentityID: memberID, WorkspaceID: wid, Role: "PlatformAdmin", ResourceType: "workspace", ActorID: admin.IdentityID, SessionID: admin.SessionID, ApprovalID: approvalID, RequestID: uuid.NewString(), Reason: "second independent administrator"}); err != nil {
		t.Fatal(err)
	}
	_, second, err := s.AuthenticateLocal(ctx, name+"-member", "auth-safety replacement credential")
	if err != nil {
		t.Fatal(err)
	}
	// Concurrent mutual disable must leave one active administrator; the loser
	// cannot use its now-revoked session to disable the survivor.
	go func() { results <- mutate(admin, "disable", second.IdentityID, wid, uuid.Nil) }()
	go func() { results <- mutate(second, "disable", admin.IdentityID, wid, uuid.Nil) }()
	successfulDisables := 0
	for range 2 {
		err := <-results
		if err == nil {
			successfulDisables++
		} else if !errors.Is(err, auth.ErrUnauthenticated) && !errors.Is(err, auth.ErrLocalProtected) {
			t.Fatal(err)
		}
	}
	if successfulDisables != 1 {
		t.Fatalf("mutual disable successes: %d", successfulDisables)
	}

	// Exercise the exact OIDC identity store used by createSession, including
	// case, trailing spaces and issuer delimiters under each database collation.
	seen := map[uuid.UUID]bool{admin.IdentityID: true}
	for _, key := range [][2]string{{"https://issuer.example", name}, {"https://issuer.example/", name}, {"https://issuer.example", strings.ToUpper(name)}, {"https://issuer.example", name + " "}, {"https://Issuer.example", name}} {
		var id uuid.UUID
		for repeat := range 2 {
			var current uuid.UUID
			err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
				var err error
				current, err = identityprofile.Upsert(ctx, tx, uuid.New(), key[0], key[1], "same@example.test", "same profile", time.Now().UTC())
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if repeat == 0 {
				id = current
				if seen[id] {
					t.Fatal("distinct Local/OIDC security identifiers merged")
				}
				seen[id] = true
			} else if id != current {
				t.Fatal("exact OIDC identity not reused")
			}
		}
		if local, err := s.HasLocalCredential(ctx, id); err != nil || local {
			t.Fatalf("OIDC identity became Local: %v %v", local, err)
		}
	}
	verified, err = m.Verify(ctx, wid)
	if err != nil || !verified.Valid || verified.Events != 5 {
		t.Fatalf("transactional lifecycle audit: %+v %v", verified, err)
	}
}

func TestAuthenticationBackendLegacyIntegration(t *testing.T) {
	b, owner := authenticationBackendFixture(t)
	if owner == nil {
		t.Fatal("owner URL required for isolated authentication fixtures")
	}
	ctx := context.Background()
	wid, other := authSafetyWorkspace(t, owner), authSafetyWorkspace(t, owner)
	s, err := auth.NewBackend(b, auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	name := "legacy-" + uuid.NewString()
	const password = "legacy-auth administrator password"
	const approverPassword = "legacy-auth independent approver password"
	id, err := s.CreateLocalCredential(ctx, name, password)
	if err != nil {
		t.Fatal(err)
	}
	err = database.Within(ctx, owner, database.ReadCommitted, func(tx database.Tx) error {
		store, err := authstore.From(tx)
		if err != nil {
			return err
		}
		if err := store.BindRole(ctx, uuid.New(), id, wid, "PlatformAdmin", time.Now().UTC()); err != nil {
			return err
		}
		return store.InsertBootstrap(ctx, id, wid, time.Now().UTC())
	})
	if err != nil {
		t.Fatal(err)
	}
	authSafetyExec(t, owner,
		`UPDATE local_auth_bootstrap SET completion_pending=true,completed_at=NULL WHERE workspace_id=$1`, wid)
	before, err := authstore.ReadCredential(ctx, b, name)
	if err != nil {
		t.Fatal(err)
	}
	complete := func(workspace uuid.UUID) (uuid.UUID, error) {
		return s.CompleteLocalBootstrap(ctx, name, password, workspace, name+"-approver", approverPassword)
	}
	if _, err := complete(other); !errors.Is(err, auth.ErrLocalInitialized) {
		t.Fatalf("legacy completion changed workspace: %v", err)
	}
	if _, err := s.BootstrapLocalAdmin(ctx, name+"-new", password, wid, name+"-approver", approverPassword); !errors.Is(err, auth.ErrLocalInitialized) {
		t.Fatalf("fresh bootstrap replaced legacy marker: %v", err)
	}
	otherID, err := s.CreateLocalCredential(ctx, name+"-existing", approverPassword)
	if err != nil {
		t.Fatal(err)
	}
	bindingID := uuid.New()
	err = database.Within(ctx, owner, database.ReadCommitted, func(tx database.Tx) error {
		store, err := authstore.From(tx)
		if err != nil {
			return err
		}
		return store.BindRole(ctx, bindingID, otherID, wid, "SecurityAdmin", time.Now().UTC())
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := complete(wid); !errors.Is(err, auth.ErrLocalInitialized) {
		t.Fatalf("legacy completion ignored an existing elevated identity: %v", err)
	}
	authSafetyExec(t, owner, `DELETE FROM role_bindings WHERE id=$1`, bindingID)
	if _, err := complete(wid); !errors.Is(err, auth.ErrLocalInitialized) {
		t.Fatalf("retired completion reopened bootstrap: %v", err)
	}
	after, err := authstore.ReadCredential(ctx, b, name)
	if err != nil || before.Hash != after.Hash || before.ID != after.ID {
		t.Fatalf("legacy completion replaced administrator: %v", err)
	}
	if _, err := complete(wid); !errors.Is(err, auth.ErrLocalInitialized) {
		t.Fatalf("legacy completion was reusable: %v", err)
	}
	if _, err := authstore.ReadCredential(ctx, b, name+"-approver"); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("retired completion created an identity: %v", err)
	}
}
