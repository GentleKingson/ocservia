package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
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
	testutil.NoError(t, err)
	authSafetyExec(t, owner, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Authentication safety test',$2,$3,$4)`, id, "auth-safety-"+id.String(), now, now)
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
	b, owner := authenticationBackendFixtureWithIsolation(t, true)
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
	testutil.NoError(t, err)
	wid := authSafetyWorkspace(t, owner)
	other := authSafetyWorkspace(t, owner)
	name := "auth-safety-" + uuid.NewString()
	const password = "auth-safety administrator credential"
	bootstrap := func() error {
		_, err := s.BootstrapLocalAdmin(ctx, name, password, wid)
		return err
	}
	drop := authSafetyAuditFailure(t, owner, wid, "local_user.bootstrap")
	if err := bootstrap(); err == nil {
		t.Fatal("partial initialization committed after audit failure")
	}
	if _, err := authstore.ReadCredential(ctx, b, name); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("failed bootstrap retained credential: %v", err)
	}
	var count int
	if err := b.QueryRow(ctx, `SELECT count(*) FROM local_auth_bootstrap`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("bootstrap must start with absent singleton: %d %v", count, err)
	}
	t.Run("absent-singleton-lock", func(t *testing.T) {
		holder, err := b.Begin(ctx, database.ReadCommitted)
		testutil.NoError(t, err)
		defer holder.Rollback(ctx)
		store, err := authstore.From(holder)
		testutil.NoError(t, err)
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
	testutil.NoError(t, err)
	if err := s.AuthorizeWorkspace(ctx, admin, other); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("cross-workspace authorization: %v", err)
	}
	if err := s.AuthorizeWorkspace(ctx, admin, wid); err != nil {
		t.Fatalf("admin workspace authorization: %v", err)
	}
}
