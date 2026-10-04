package mysql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

func TestRealAuthenticationExpiryAfterLockWait(t *testing.T) {
	b, _, _ := migrateFixture(t)
	ctx := context.Background()
	identity, session, lease := uuid.New(), uuid.New(), uuid.New()
	username := "expiry-" + uuid.NewString()
	if err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		store, err := authstore.From(tx)
		if err != nil {
			return err
		}
		if err = store.InsertCredential(ctx, identity, username, "opaque-test-hash", time.Now().UTC()); err != nil {
			return err
		}
		return store.InsertSession(ctx, session, identity, time.Now().UTC().Add(time.Hour), false, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"lease", "session"} {
		t.Run(kind, func(t *testing.T) {
			blocker, err := b.Begin(ctx, database.ReadCommitted)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(ctx)
			if kind == "lease" {
				_, err = blocker.Exec(ctx, `INSERT INTO local_auth_attempts(username,window_until,expires_at,lease_id,lease_until) VALUES(?,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))+900000000,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))+900000000,?,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))+150000)`, username, UUIDBytes(lease))
			} else {
				_, err = blocker.Exec(ctx, `UPDATE auth_sessions SET expires_at=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))+150000 WHERE id=?`, UUIDBytes(session))
			}
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			finished := make(chan error, 1)
			go func() {
				timeout, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				finished <- database.Within(timeout, b, database.ReadCommitted, func(tx database.Tx) error {
					store, err := authstore.From(tx)
					if err != nil {
						return err
					}
					close(started)
					if kind == "session" {
						err = store.LockActiveSession(timeout, identity, session, true)
						if !errors.Is(err, database.ErrNotFound) {
							return errors.New("expired session passed locked authorization")
						}
						return nil
					}
					cleared, err := store.ClearAttempt(timeout, username, lease)
					if err != nil {
						return err
					}
					if cleared {
						return errors.New("expired single-flight lease was consumed")
					}
					return nil
				})
			}()
			select {
			case <-started:
			case err := <-finished:
				t.Fatalf("start waiter: %v", err)
			case <-time.After(6 * time.Second):
				t.Fatal("waiter did not start")
			}
			select {
			case err := <-finished:
				t.Fatalf("did not wait for locked %s: %v", kind, err)
			case <-time.After(300 * time.Millisecond):
			}
			if err = blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-finished; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRealAuthenticationExactKeyLockOrder(t *testing.T) {
	b, _, _ := migrateFixture(t)
	ctx := context.Background()
	identity := uuid.New()
	if err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		store, err := authstore.From(tx)
		if err != nil {
			return err
		}
		return store.InsertCredential(ctx, identity, "order-"+uuid.NewString(), "opaque-test-hash", time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"provision", "management"} {
		t.Run(kind, func(t *testing.T) {
			guard, err := b.Begin(ctx, database.ReadCommitted)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Rollback(ctx)
			if err = LockExactKey(ctx, guard, "identities"); err != nil {
				t.Fatal(err)
			}
			id := identity
			if kind == "provision" {
				id = uuid.New()
			}
			started := make(chan struct{})
			finished := make(chan error, 1)
			go func() {
				bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				finished <- database.Within(bounded, b, database.ReadCommitted, func(tx database.Tx) error {
					store, err := authstore.From(tx)
					if err != nil {
						return err
					}
					close(started)
					if kind == "provision" {
						return store.InsertCredential(bounded, id, "order-"+uuid.NewString(), "opaque-test-hash", time.Now().UTC())
					}
					if err = store.LockManagement(bounded); err != nil {
						return err
					}
					if err = store.LockLocalIdentity(bounded, id); err != nil {
						return err
					}
					return store.Disable(bounded, id, time.Now().UTC())
				})
			}()
			select {
			case <-started:
			case err := <-finished:
				t.Fatalf("start writer: %v", err)
			}
			select {
			case err := <-finished:
				t.Fatalf("writer bypassed exact guard: %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			probe, err := b.Begin(ctx, database.ReadCommitted)
			if err != nil {
				t.Fatal(err)
			}
			var raw []byte
			err = probe.QueryRow(ctx, `SELECT id FROM identities WHERE id=? FOR UPDATE NOWAIT`, UUIDBytes(id)).Scan(&raw)
			rollback := probe.Rollback(ctx)
			if kind == "provision" && !errors.Is(err, database.ErrNotFound) {
				t.Fatalf("provision locked identity before exact guard: %v", err)
			}
			if kind == "management" && err != nil {
				t.Fatalf("management locked identity before exact guard: %v", err)
			}
			if rollback != nil {
				t.Fatal(rollback)
			}
			if err = guard.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-finished; err != nil {
				t.Fatal(err)
			}
		})
	}
}
