package auth

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLocalR4ConcurrencyIntegration(t *testing.T) {
	url := os.Getenv("OCSERV_TEST_DATABASE_URL")
	ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if url == "" || ownerURL == "" {
		t.Skip("isolated runtime and owner databases required")
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
	s, err := New(ctx, pool, Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	wid := uuid.Must(uuid.NewV7())
	name := "r4-" + uuid.NewString()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'R4',$2,now(),now())`, wid, name)
	defer func() {
		_, _ = owner.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS r4_fail_approver, DROP CONSTRAINT IF EXISTS r4_fail_password`)
		_, _ = owner.Exec(ctx, `DELETE FROM local_auth_bootstrap WHERE workspace_id=$1`, wid)
		_, _ = owner.Exec(ctx, `DELETE FROM role_bindings WHERE workspace_id=$1`, wid)
	}()
	const password = "r4-administrator-secret"
	const secondPassword = "r4-independent-approver-secret"
	bootstrap := func() (uuid.UUID, error) {
		return s.BootstrapLocalAdmin(ctx, name, password, wid, name+"-approver", secondPassword)
	}
	exec(`ALTER TABLE audit_events ADD CONSTRAINT r4_fail_approver CHECK(action<>'local_user.bootstrap-approver') NOT VALID`)
	if _, err := bootstrap(); err == nil {
		t.Fatal("approver audit failure committed")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM identities WHERE subject IN ($1,$2))+(SELECT count(*) FROM local_auth_bootstrap)`, name, name+"-approver").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial initialization %d %v", count, err)
	}
	exec(`ALTER TABLE audit_events DROP CONSTRAINT r4_fail_approver`)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := bootstrap(); results <- err }()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrLocalInitialized) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("concurrent bootstrap successes %d", success)
	}
	_, admin, err := s.AuthenticateLocal(ctx, name, password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteLocalBootstrap(ctx, name, password, wid, name+"-extra", secondPassword); !errors.Is(err, ErrLocalInitialized) {
		t.Fatalf("completed bootstrap reopened: %v", err)
	}
	userName := name + "-member"
	memberID, err := s.MutateLocalUser(ctx, LocalUserMutation{Action: "create", Username: userName, Password: password, ActorID: admin.IdentityID, SessionID: admin.SessionID, WorkspaceID: wid, RequestID: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	cookie, member, err := s.AuthenticateLocal(ctx, userName, password)
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range []Principal{{Issuer: "https://oidc.example", IdentityID: memberID, SessionID: member.SessionID}, {Issuer: LocalIssuer, BreakGlass: true, IdentityID: memberID, SessionID: member.SessionID}} {
		if err := s.ChangeLocalPassword(ctx, actor, password, "r4-new-member-password", uuid.NewString()); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("non-Local change: %v", err)
		}
	}
	for range 5 {
		if err := s.ChangeLocalPassword(ctx, member, "incorrect-current-secret", "r4-new-member-password", uuid.NewString()); !errors.Is(err, ErrUnauthenticated) {
			t.Fatal(err)
		}
	}
	var blocked bool
	if err := pool.QueryRow(ctx, `SELECT failures=5 AND blocked_until>clock_timestamp() FROM local_auth_attempts WHERE username=$1`, userName).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("self change backoff: %v %v", blocked, err)
	}
	exec(`UPDATE local_auth_attempts SET blocked_until='-infinity' WHERE username=$1`, userName)
	exec(`ALTER TABLE audit_events ADD CONSTRAINT r4_fail_password CHECK(action<>'local_user.change-password') NOT VALID`)
	if err := s.ChangeLocalPassword(ctx, member, password, "r4-new-member-password", uuid.NewString()); err == nil {
		t.Fatal("audit failure committed password")
	}
	if _, err := s.Authenticate(ctx, cookie); err != nil {
		t.Fatalf("audit rollback session: %v", err)
	}
	exec(`ALTER TABLE audit_events DROP CONSTRAINT r4_fail_password`)
	// Disable races the complete KDF + transaction path, not a direct SQL reset.
	start := make(chan struct{})
	results = make(chan error, 2)
	go func() {
		<-start
		results <- s.ChangeLocalPassword(ctx, member, password, "r4-new-member-password", uuid.NewString())
	}()
	go func() {
		<-start
		_, err := s.MutateLocalUser(ctx, LocalUserMutation{Action: "disable", IdentityID: memberID, ActorID: admin.IdentityID, SessionID: admin.SessionID, WorkspaceID: wid, RequestID: uuid.NewString()})
		results <- err
	}()
	close(start)
	for range 2 {
		if err := <-results; err != nil && !errors.Is(err, ErrUnauthenticated) {
			t.Fatal(err)
		}
	}
	if _, err := s.Authenticate(ctx, cookie); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("disable race session: %v", err)
	}
	for _, secret := range []string{password, "r4-new-member-password"} {
		if _, _, err := s.AuthenticateLocal(ctx, userName, secret); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("disabled login: %v", err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE identity_id=$1 AND revoked_at IS NULL`, memberID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("race left active sessions %d %v", count, err)
	}
}
