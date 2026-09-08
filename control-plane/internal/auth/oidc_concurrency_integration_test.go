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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pause after the upsert has returned, while its transaction still owns the row.
type oidcUpsertBarrier struct{ locked, release chan struct{} }
type oidcUpsertKey struct{}

func (b *oidcUpsertBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, oidcUpsertKey{}, strings.HasPrefix(data.SQL, "INSERT INTO identities"))
}
func (b *oidcUpsertBarrier) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(oidcUpsertKey{}) == true {
		close(b.locked)
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	}
}

func TestOIDCDisableCommitOrdersIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	for _, loginFirst := range []bool{false, true} {
		name := "disable-first"
		if loginFirst {
			name = "login-first"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			pool, err := pgxpool.New(ctx, databaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			id := uuid.New()
			issuer := "https://concurrency.example/" + id.String()
			if _, err := pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,$2,'subject',now(),now())`, id, issuer); err != nil {
				t.Fatal(err)
			}
			cfg, err := pgxpool.ParseConfig(databaseURL)
			if err != nil {
				t.Fatal(err)
			}
			cfg.ConnConfig.RuntimeParams["application_name"] = id.String()
			barrier := &oidcUpsertBarrier{make(chan struct{}), make(chan struct{})}
			defer close(barrier.release)
			if loginFirst {
				cfg.ConnConfig.Tracer = barrier
			}
			loginPool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer loginPool.Close()
			service, err := New(ctx, loginPool, Config{SessionKey: make([]byte, 32), SessionTTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			disable, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer disable.Rollback(context.Background())
			disablePID := disable.Conn().PgConn().PID()
			if !loginFirst {
				if _, err := disable.Exec(ctx, `UPDATE identities SET disabled_at=now() WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			type result struct {
				cookie *http.Cookie
				err    error
			}
			loggedIn := make(chan result, 1)
			go func() {
				cookie, _, err := service.createSession(ctx, issuer, "subject", "", "", false, nil)
				loggedIn <- result{cookie, err}
			}()
			if loginFirst {
				select {
				case <-barrier.locked:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				disabled := make(chan error, 1)
				go func() {
					_, err := disable.Exec(ctx, `UPDATE identities SET disabled_at=now() WHERE id=$1`, id)
					if err == nil {
						err = disable.Commit(ctx)
					}
					disabled <- err
				}()
				waitOIDCLock(t, ctx, pool, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, disablePID)
				barrier.release <- struct{}{}
				if err := <-disabled; err != nil {
					t.Fatal(err)
				}
			} else {
				waitOIDCLock(t, ctx, pool, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock')`, id.String())
				if err := disable.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			got := <-loggedIn
			if loginFirst && (got.err != nil || got.cookie == nil) {
				t.Fatalf("login-first: %v", got.err)
			}
			if !loginFirst && (!errors.Is(got.err, ErrUnauthenticated) || got.cookie != nil) {
				t.Fatalf("disable-first: %v", got.err)
			}
			if _, err := service.Authenticate(ctx, got.cookie); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("disabled session accepted: %v", err)
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE identity_id=$1`, id).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if loginFirst {
				want = 1
			}
			if count != want {
				t.Fatalf("sessions=%d, want %d", count, want)
			}
		})
	}
}

func waitOIDCLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, arg any) {
	t.Helper()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, query, arg).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
