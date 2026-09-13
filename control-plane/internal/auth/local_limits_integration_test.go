package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type localLimitFixture struct {
	*Service
	pool *pgxpool.Pool
}

func localLimitService(t *testing.T) *localLimitFixture {
	t.Helper()
	url := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s, err := NewBackend(postgres.WrapPool(pool), Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return &localLimitFixture{Service: s, pool: pool}
}

func localLimitExec(t *testing.T, s *localLimitFixture, sql string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func localLimitState(t *testing.T, s *localLimitFixture, name string) (int, bool) {
	t.Helper()
	var failures int
	var active bool
	if err := s.pool.QueryRow(context.Background(), `SELECT failures,lease_id IS NOT NULL FROM local_auth_attempts WHERE username=$1`, name).Scan(&failures, &active); err != nil {
		t.Fatal(err)
	}
	return failures, active
}

func TestLocalAccountBackoffIntegration(t *testing.T) {
	s, other := localLimitService(t), localLimitService(t)
	ctx := context.Background()
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprintf("known=%t", known), func(t *testing.T) {
			name := "r2-" + uuid.NewString()
			t.Cleanup(func() { localLimitExec(t, s, `DELETE FROM local_auth_attempts WHERE username=$1`, name) })
			if known {
				if _, err := s.CreateLocalCredential(ctx, name, "r2 account test password"); err != nil {
					t.Fatal(err)
				}
			}
			for i := 1; i <= 5; i++ {
				if _, _, err := other.AuthenticateLocal(ctx, " "+strings.ToUpper(name)+" ", "wrong"); !errors.Is(err, ErrUnauthenticated) {
					t.Fatal(err)
				}
				if failures, active := localLimitState(t, s, name); failures != i || active {
					t.Fatalf("state: %d %t", failures, active)
				}
			}
			// Hold cooldown well beyond the duration of this test; rejected requests
			// must not mutate the deadline, observation window, lease or count.
			localLimitExec(t, s, `UPDATE local_auth_attempts SET blocked_until=clock_timestamp()+interval '5 minutes' WHERE username=$1`, name)
			var before, after string
			if err := s.pool.QueryRow(ctx, `SELECT row_to_json(a)::text FROM local_auth_attempts a WHERE username=$1`, name).Scan(&before); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 8; i++ {
				if _, _, err := s.AuthenticateLocal(ctx, name, "r2 account test password"); !errors.Is(err, ErrUnauthenticated) {
					t.Fatal(err)
				}
			}
			if err := s.pool.QueryRow(ctx, `SELECT row_to_json(a)::text FROM local_auth_attempts a WHERE username=$1`, name).Scan(&after); err != nil || before != after {
				t.Fatalf("rejection changed state: %v", err)
			}
			for i := 6; i <= 16; i++ {
				localLimitExec(t, s, `UPDATE local_auth_attempts SET blocked_until=clock_timestamp()-interval '1 second' WHERE username=$1`, name)
				lease, err := other.reserveLocalAttempt(ctx, name)
				if err != nil {
					t.Fatalf("expired cooldown: %v", err)
				}
				if err := other.finishLocalAttempt(name, lease, true); err != nil {
					t.Fatal(err)
				}
				var delay float64
				if err := s.pool.QueryRow(ctx, `SELECT extract(epoch FROM blocked_until-clock_timestamp())::float8 FROM local_auth_attempts WHERE username=$1`, name).Scan(&delay); err != nil {
					t.Fatal(err)
				}
				want := min(300, 1<<min(i-5, 9))
				if delay > float64(want) || delay < float64(want)-1 {
					t.Fatalf("failure %d delay %f want %d", i, delay, want)
				}
			}
			localLimitExec(t, s, `UPDATE local_auth_attempts SET window_until=clock_timestamp()-interval '1 second',blocked_until=clock_timestamp()-interval '1 second',expires_at=clock_timestamp()+interval '1 minute' WHERE username=$1`, name)
			renewed, err := s.reserveLocalAttempt(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			var fullWindow bool
			if err := s.pool.QueryRow(ctx, `SELECT failures=0 AND expires_at>clock_timestamp()+interval '14 minutes' FROM local_auth_attempts WHERE username=$1`, name).Scan(&fullWindow); err != nil || !fullWindow {
				t.Fatalf("window renewal: %t %v", fullWindow, err)
			}
			if err := s.finishLocalAttempt(name, renewed, false); err != nil {
				t.Fatal(err)
			}
			localLimitExec(t, s, `UPDATE local_auth_attempts SET expires_at=clock_timestamp()-interval '1 second' WHERE username=$1`, name)
			lease, err := s.reserveLocalAttempt(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			if failures, active := localLimitState(t, s, name); failures != 0 || !active {
				t.Fatalf("expiry did not reset: %d %t", failures, active)
			}
			if err := s.finishLocalAttempt(name, lease, false); err != nil {
				t.Fatal(err)
			}
			if known {
				for i := 0; i < 3; i++ {
					if _, _, err := s.AuthenticateLocal(ctx, name, "r2 account test password"); err != nil {
						t.Fatal(err)
					}
				}
				var count int
				if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM local_auth_attempts WHERE username=$1`, name).Scan(&count); err != nil || count != 0 {
					t.Fatalf("success left failures: %d %v", count, err)
				}
			}
		})
	}
}

func TestLocalAccountConcurrentLeaseAndFencingIntegration(t *testing.T) {
	s, other := localLimitService(t), localLimitService(t)
	ctx := context.Background()
	name := "r2-" + uuid.NewString()
	t.Cleanup(func() { localLimitExec(t, s, `DELETE FROM local_auth_attempts WHERE username=$1`, name) })
	if _, err := s.CreateLocalCredential(ctx, name, "r2 concurrency test password"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	leases := make(chan uuid.UUID, 20)
	errs := make(chan error, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(svc *Service) {
			defer wg.Done()
			<-start
			lease, err := svc.reserveLocalAttempt(ctx, name)
			if err == nil {
				leases <- lease
			} else if !errors.Is(err, ErrUnauthenticated) {
				errs <- err
			}
		}([]*Service{s.Service, other.Service}[i%2])
	}
	close(start)
	wg.Wait()
	close(leases)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(leases) != 1 {
		t.Fatalf("concurrent admissions: %d", len(leases))
	}
	old := <-leases
	credential, err := s.localCredential(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	credential.attemptLease = old
	// Simulate a process that exits without completion; only the lease, not a
	// password-failure counter, remains. Expiry allows a new fenced attempt.
	localLimitExec(t, s, `UPDATE local_auth_attempts SET lease_until=clock_timestamp()-interval '1 second' WHERE username=$1`, name)
	next, err := other.reserveLocalAttempt(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.createSession(ctx, LocalIssuer, name, "", "", false, &credential); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("stale success: %v", err)
	}
	if err := s.finishLocalAttempt(name, old, true); err != nil {
		t.Fatal(err)
	}
	if failures, active := localLimitState(t, s, name); failures != 0 || !active {
		t.Fatalf("stale completion changed new lease: %d %t", failures, active)
	}
	if err := other.finishLocalAttempt(name, next, true); err != nil {
		t.Fatal(err)
	}
	if failures, active := localLimitState(t, s, name); failures != 1 || active {
		t.Fatalf("new completion: %d %t", failures, active)
	}
	// A new state after a reset/disable deletion must also reject old success.
	localLimitExec(t, s, `DELETE FROM local_auth_attempts WHERE username=$1`, name)
	next, err = other.reserveLocalAttempt(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.finishLocalAttempt(name, next, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.createSession(ctx, LocalIssuer, name, "", "", false, &credential); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("previous epoch success: %v", err)
	}
	if failures, _ := localLimitState(t, s, name); failures != 1 {
		t.Fatal("old success cleared new failures")
	}
}

// Tracing injects cancellation at the actual credential-read boundary without
// sleeping or adding test hooks to authentication production code.
type localCancelRead struct {
	cancel context.CancelFunc
	after  bool
}
type localReadKey struct{}

func (c localCancelRead) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT i.id,c.password_hash") {
		if !c.after {
			c.cancel()
		}
		return context.WithValue(ctx, localReadKey{}, true)
	}
	return ctx
}
func (c localCancelRead) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if c.after && ctx.Value(localReadKey{}) == true {
		c.cancel()
	}
}

func TestLocalAccountCancellationAndErrorsIntegration(t *testing.T) {
	s := localLimitService(t)
	name := "r2-" + uuid.NewString()
	ctx := context.Background()
	t.Cleanup(func() { localLimitExec(t, s, `DELETE FROM local_auth_attempts WHERE username=$1`, name) })
	if _, err := s.CreateLocalCredential(ctx, name, "r2 cancellation test password"); err != nil {
		t.Fatal(err)
	}
	for _, after := range []bool{false, true} {
		requestCtx, cancel := context.WithCancel(ctx)
		cfg, err := pgxpool.ParseConfig(os.Getenv("OCSERV_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.Tracer = localCancelRead{cancel: cancel, after: after}
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		other := *s.Service
		other.backend = postgres.WrapPool(pool)
		_, _, err = other.AuthenticateLocal(requestCtx, name, "wrong")
		cancel()
		pool.Close()
		if err == nil {
			t.Fatal("cancelled login succeeded")
		}
		want := 0
		if after {
			want = 1
		}
		if failures, active := localLimitState(t, s, name); failures != want || active {
			t.Fatalf("cancel after read=%t: failures=%d active=%t err=%v", after, failures, active, err)
		}
	}
	localLimitExec(t, s, `UPDATE local_credentials SET password_hash='corrupt' WHERE username=$1`, name)
	if _, _, err := s.AuthenticateLocal(ctx, name, "wrong"); !errors.Is(err, errInvalidPasswordHash) {
		t.Fatalf("hash fault: %v", err)
	}
	if failures, active := localLimitState(t, s, name); failures != 1 || active {
		t.Fatal("hash fault counted as password failure")
	}
	// A denied cleanup must abort admission, not bypass account protection.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE local_auth_attempts SET expires_at=clock_timestamp()-interval '1 second' WHERE username=$1`, name); err != nil {
		t.Fatal(err)
	}
	// Block the admission transaction using its shared capacity lock.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, localAttemptLockID); err != nil {
		t.Fatal(err)
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := s.reserveLocalAttempt(deadlineCtx, name); err == nil {
		t.Fatal("DB lock timeout admitted login")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	broken := localLimitService(t)
	broken.pool.Close()
	if _, _, err := broken.AuthenticateLocal(ctx, name, "wrong"); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("database unavailable admitted login: %v", err)
	}
	if err := broken.finishLocalAttempt(name, uuid.New(), true); err == nil {
		t.Fatal("completion DB failure hidden")
	}
	if failures, active := localLimitState(t, s, name); failures != 1 || active {
		t.Fatal("DB failure changed failure count")
	}
}

func TestLocalAccountCapacityAndCleanupIntegration(t *testing.T) {
	s, other := localLimitService(t), localLimitService(t)
	ctx := context.Background()
	prefix := "r2-cap-" + uuid.NewString() + "-"
	t.Cleanup(func() { localLimitExec(t, s, `DELETE FROM local_auth_attempts WHERE username LIKE $1`, prefix+"%") })
	// This test needs a disposable DB, as it deliberately fills the global
	// bounded table. Other fixtures' live rows are retained, not evicted.
	localLimitExec(t, s, `INSERT INTO local_auth_attempts(username,window_until,expires_at)
		SELECT $1||n,clock_timestamp()+interval '15 minutes',clock_timestamp()+interval '15 minutes'
		FROM generate_series(1,$2-(SELECT count(*) FROM local_auth_attempts)) n`, prefix, localAttemptCapacity-1)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := []*Service{s.Service, other.Service}[i%2].reserveLocalAttempt(ctx, prefix+uuid.NewString())
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	admitted := 0
	for err := range errs {
		if err == nil {
			admitted++
		} else if !errors.Is(err, errLocalAttemptCapacity) {
			t.Error(err)
		}
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM local_auth_attempts`).Scan(&count); err != nil || count != localAttemptCapacity || admitted != 1 {
		t.Fatalf("capacity: rows=%d admitted=%d err=%v", count, admitted, err)
	}
	localLimitExec(t, s, `UPDATE local_auth_attempts SET expires_at=clock_timestamp()-interval '1 second' WHERE username LIKE $1`, prefix+"%")
	if _, err := other.reserveLocalAttempt(ctx, prefix+"recovered"); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM local_auth_attempts WHERE username LIKE $1`, prefix+"%").Scan(&count); err != nil || count != 1 {
		t.Fatalf("cleanup: %d %v", count, err)
	}
}

func TestLocalAccountDatabasePermissionsFailClosedIntegration(t *testing.T) {
	s := localLimitService(t)
	ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if ownerURL == "" {
		t.Skip("OCSERV_TEST_OWNER_DATABASE_URL is not set")
	}
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	var role, ownerRole string
	if err := s.pool.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `SELECT current_user`).Scan(&ownerRole); err != nil {
		t.Fatal(err)
	}
	if role == ownerRole {
		t.Skip("requires a separate restricted runtime role")
	}
	name := "r2-db-" + uuid.NewString()
	t.Cleanup(func() { localLimitExec(t, s, `DELETE FROM local_auth_attempts WHERE username=$1`, name) })
	for _, test := range []struct{ privilege, table string }{{"SELECT", "local_credentials"}, {"DELETE", "local_auth_attempts"}} {
		t.Run(test.table, func(t *testing.T) {
			if _, err := owner.Exec(ctx, "REVOKE "+test.privilege+" ON "+test.table+" FROM "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := owner.Exec(ctx, "GRANT "+test.privilege+" ON "+test.table+" TO "+pgx.Identifier{role}.Sanitize()); err != nil {
					t.Fatal(err)
				}
			}()
			if _, _, err := s.AuthenticateLocal(ctx, name, "wrong"); err == nil || errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("DB/cleanup fault not propagated: %v", err)
			}
		})
	}
	if failures, active := localLimitState(t, s, name); failures != 0 || active {
		t.Fatalf("DB error counted as password failure: %d %t", failures, active)
	}
}
