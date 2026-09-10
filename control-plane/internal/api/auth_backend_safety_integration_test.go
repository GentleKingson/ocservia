package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/identityprofile"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/google/uuid"
)

// Fixture SQL is backend-specific; every business action below uses the same
// service and the restricted runtime connection on all three engines.
func authSafetySQL(b database.Backend, pg, my string, args []any) (string, []any) {
	if _, ok := b.(*mysql.Backend); !ok {
		return pg, args
	}
	args = append([]any(nil), args...)
	for i, arg := range args {
		if id, ok := arg.(uuid.UUID); ok {
			args[i] = mysql.UUIDBytes(id)
		}
	}
	return my, args
}

func authSafetyExec(t *testing.T, b database.Backend, pg, my string, args ...any) {
	t.Helper()
	query, args := authSafetySQL(b, pg, my, args)
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
	authSafetyExec(t, owner,
		`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'PR03',$2,$3,$4)`,
		`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'PR03',?,?,?)`, id, "pr03-"+id.String(), now, now)
	t.Cleanup(func() {
		authSafetyExec(t, owner, `DELETE FROM local_auth_bootstrap WHERE workspace_id=$1`, `DELETE FROM local_auth_bootstrap WHERE workspace_id=?`, id)
		authSafetyExec(t, owner, `DELETE FROM role_bindings WHERE workspace_id=$1`, `DELETE FROM role_bindings WHERE workspace_id=?`, id)
	})
	return id
}

func authSafetyAuditFailure(t *testing.T, owner database.Backend, workspace uuid.UUID, action string) func() {
	t.Helper()
	pg := `ALTER TABLE audit_events ADD CONSTRAINT pr03_audit_failure CHECK(workspace_id <> '` + workspace.String() + `' OR action <> '` + action + `')`
	my := `ALTER TABLE audit_events ADD CONSTRAINT pr03_audit_failure CHECK(workspace_id <> X'` + strings.ReplaceAll(workspace.String(), "-", "") + `' OR action <> '` + action + `')`
	authSafetyExec(t, owner, pg, my)
	removed := false
	drop := func() {
		if removed {
			return
		}
		my := `ALTER TABLE audit_events DROP CONSTRAINT pr03_audit_failure`
		if os.Getenv("PR02_ENGINE") == "mysql" {
			my = `ALTER TABLE audit_events DROP CHECK pr03_audit_failure`
		}
		authSafetyExec(t, owner, `ALTER TABLE audit_events DROP CONSTRAINT pr03_audit_failure`, my)
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
	query, _ := authSafetySQL(b, `SELECT current_user`, `SELECT CURRENT_USER()`, nil)
	if err := b.QueryRow(ctx, query).Scan(&user); err != nil || !strings.HasPrefix(user, "ocservia_app") {
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
			authSafetyExec(t, b, `DELETE FROM local_auth_attempts WHERE username LIKE 'pr03-capacity-%'`, `DELETE FROM local_auth_attempts WHERE username LIKE 'pr03-capacity-%'`)
		})
		results := make(chan error, 16)
		for i := range cap(results) {
			go func() {
				results <- database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
					store, err := authstore.From(tx)
					if err != nil {
						return err
					}
					admitted, err := store.ReserveAttempt(ctx, fmt.Sprintf("pr03-capacity-%d", i), uuid.New(), baseline+8, 15*time.Minute, 30*time.Second)
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
	name := "pr03-" + uuid.NewString()
	const password = "pr03 administrator authentication secret"
	const approverPassword = "pr03 independent approver authentication secret"
	bootstrap := func() error {
		_, err := s.BootstrapLocalAdmin(ctx, name, password, wid, name+"-approver", approverPassword)
		return err
	}
	drop := authSafetyAuditFailure(t, owner, wid, "local_user.bootstrap-approver")
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
	if err != nil || !verified.Valid || verified.Events != 2 {
		t.Fatalf("bootstrap audit rollback: %+v %v", verified, err)
	}
	_, admin, err := s.AuthenticateLocal(ctx, name, password)
	if err != nil {
		t.Fatal(err)
	}
	_, approver, err := s.AuthenticateLocal(ctx, name+"-approver", approverPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteLocalBootstrap(ctx, name, password, wid, name+"-extra", approverPassword); !errors.Is(err, auth.ErrLocalInitialized) {
		t.Fatalf("completed bootstrap reopened: %v", err)
	}
	mutate := func(actor auth.Principal, action string, id, workspace, approval uuid.UUID) error {
		_, err := s.MutateLocalUser(ctx, auth.LocalUserMutation{Action: action, IdentityID: id, ActorID: actor.IdentityID, SessionID: actor.SessionID, WorkspaceID: workspace, ApprovalID: approval, Password: "pr03 replacement credential secret", RequestID: uuid.NewString()})
		return err
	}
	for _, id := range []uuid.UUID{admin.IdentityID, approver.IdentityID} {
		if err := mutate(admin, "disable", id, wid, uuid.Nil); !errors.Is(err, auth.ErrLocalProtected) {
			t.Fatalf("last management pair not protected: %v", err)
		}
	}
	roles := rbac.NewBackend(b)
	if err := roles.Authorize(ctx, admin.IdentityID, "local_user.manage", rbac.Resource{WorkspaceID: other, Type: "workspace"}, false); !errors.Is(err, rbac.ErrForbidden) {
		t.Fatalf("cross-workspace authorization: %v", err)
	}
	if err := mutate(admin, "disable", approver.IdentityID, other, uuid.Nil); !errors.Is(err, auth.ErrLocalInvalid) {
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
	approvalService := approvals.NewBackend(b)
	approve := func(action, kind string, id uuid.UUID, hash []byte, summary []byte) uuid.UUID {
		t.Helper()
		request, err := approvalService.Create(ctx, approvals.Request{WorkspaceID: wid, RequesterID: admin.IdentityID, SessionID: admin.SessionID, ResourceID: id, Action: action, ResourceType: kind, Reason: "PR03 review", TTL: time.Hour, RequestID: uuid.NewString(), RequestHash: hash, RequestSummary: summary, AuthorityResources: []approvals.AuthorityResource{{WorkspaceID: wid, Type: "workspace"}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = approvalService.Approve(ctx, approvals.Decision{ApprovalID: request.ID, ApproverID: approver.IdentityID, SessionID: approver.SessionID, Reason: "independent review", RequestID: uuid.NewString(), ExpectedRequestHash: request.RequestHash}); err != nil {
			t.Fatal(err)
		}
		return request.ID
	}
	hash, summary := approvals.GenericBinding("local_user.reset-password", "local_user", memberID)
	approvalID := approve("local_user.reset-password", "local_user", memberID, hash, summary)
	for _, key := range [][2]string{{"LOCAL_USER.RESET-PASSWORD", "local_user"}, {"local_user.reset-password ", "local_user"}, {"local_user.reset-password", "LOCAL_USER"}, {"local_user.reset-password", "local_user "}} {
		err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
			return approvals.ConsumeBoundTx(ctx, tx, approvalID, wid, admin.IdentityID, key[0], key[1], memberID, hash)
		})
		if !errors.Is(err, approvals.ErrNotReady) {
			t.Fatalf("approval identifiers collapsed by collation: %v", err)
		}
	}
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
	request, err := approvalService.Get(ctx, approvalID)
	if err != nil || request.Status != "approved" {
		t.Fatalf("approval consumption escaped rollback: %+v %v", request, err)
	}
	drop()
	if err := mutate(admin, "reset-password", memberID, wid, approvalID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, memberCookie); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("reset retained old session: %v", err)
	}
	if err := mutate(admin, "reset-password", memberID, wid, approvalID); !errors.Is(err, approvals.ErrNotReady) {
		t.Fatalf("reset approval replay: %v", err)
	}

	hash, summary = rbac.BindingApprovalContent(memberID, wid, "PlatformAdmin", "workspace", uuid.Nil)
	approvalID = approve("role_binding.elevate", "role_binding", memberID, hash, summary)
	if _, err := roles.CreateBinding(ctx, rbac.BindingRequest{IdentityID: memberID, WorkspaceID: wid, Role: "PlatformAdmin", ResourceType: "workspace", ActorID: admin.IdentityID, SessionID: admin.SessionID, ApprovalID: approvalID, RequestID: uuid.NewString(), Reason: "second independent administrator"}); err != nil {
		t.Fatal(err)
	}
	_, second, err := s.AuthenticateLocal(ctx, name+"-member", "pr03 replacement credential secret")
	if err != nil {
		t.Fatal(err)
	}
	// Two administrators are also a valid independent pair. Once the third
	// identity is disabled, neither concurrent mutation may remove that pair.
	if err := mutate(admin, "disable", approver.IdentityID, wid, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	go func() { results <- mutate(admin, "disable", second.IdentityID, wid, uuid.Nil) }()
	go func() { results <- mutate(second, "disable", admin.IdentityID, wid, uuid.Nil) }()
	for range 2 {
		if err := <-results; !errors.Is(err, auth.ErrLocalProtected) {
			t.Fatalf("concurrent management pair lost: %v", err)
		}
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
	if err != nil || !verified.Valid || verified.Events != 10 {
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
	const password = "pr03 existing legacy administrator password"
	const approverPassword = "pr03 independent legacy completion password"
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
		`UPDATE local_auth_bootstrap SET completion_pending=true,completed_at=NULL WHERE workspace_id=$1`,
		`UPDATE local_auth_bootstrap SET completion_pending=true,completed_at=NULL WHERE workspace_id=?`, wid)
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
	authSafetyExec(t, owner, `DELETE FROM role_bindings WHERE id=$1`, `DELETE FROM role_bindings WHERE id=?`, bindingID)
	drop := authSafetyAuditFailure(t, owner, wid, "local_user.bootstrap-approver")
	if _, err := complete(wid); err == nil {
		t.Fatal("legacy completion escaped audit rollback")
	}
	drop()
	if completed, err := complete(wid); err != nil || completed != id {
		t.Fatalf("eligible legacy completion: %s %v", completed, err)
	}
	after, err := authstore.ReadCredential(ctx, b, name)
	if err != nil || before.Hash != after.Hash || before.ID != after.ID {
		t.Fatalf("legacy completion replaced administrator: %v", err)
	}
	if _, err := complete(wid); !errors.Is(err, auth.ErrLocalInitialized) {
		t.Fatalf("legacy completion was reusable: %v", err)
	}
	verified, err := audit.NewBackendManager(b, bytes.Repeat([]byte{7}, 32)).Verify(ctx, wid)
	if err != nil || !verified.Valid || verified.Events != 1 {
		t.Fatalf("legacy completion audit: %+v %v", verified, err)
	}
}
