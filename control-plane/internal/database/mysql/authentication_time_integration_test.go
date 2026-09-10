package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func TestRealAuthenticationFullTimeWrites(t *testing.T) {
	b, _, _ := migrateFixture(t)
	ctx := context.Background()
	for _, micros := range []int64{value.MinTimestamp, -1, 0, value.EndTimestamp - 2} {
		stamp := value.Timestamp{Micros: micros, Valid: true}
		now, err := stamp.Time()
		if err != nil {
			t.Fatal(err)
		}
		identity, session := uuid.New(), uuid.New()
		if err = database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
			store, err := authstore.From(tx)
			if err != nil {
				return err
			}
			if err = store.InsertCredential(ctx, identity, "time-"+identity.String(), "initial", now); err != nil {
				return err
			}
			if err = store.SetPassword(ctx, identity, "changed", now); err != nil {
				return err
			}
			if err = store.InsertSession(ctx, session, identity, now.Add(time.Microsecond), false, now); err != nil {
				return err
			}
			if err = store.RevokeIdentitySessions(ctx, identity, now); err != nil {
				return err
			}
			return store.Disable(ctx, identity, now)
		}); err != nil {
			t.Fatal(micros, err)
		}
		var got [8]value.Timestamp
		if err = b.QueryRow(ctx, `SELECT i.created_at,i.updated_at,i.disabled_at,c.created_at,c.updated_at,c.password_changed_at,s.created_at,s.revoked_at FROM identities i JOIN local_credentials c ON c.identity_id=i.id JOIN auth_sessions s ON s.identity_id=i.id WHERE i.id=?`, UUIDBytes(identity)).Scan(&got[0], &got[1], &got[2], &got[3], &got[4], &got[5], &got[6], &got[7]); err != nil {
			t.Fatal(err)
		}
		for _, actual := range got {
			if actual != stamp {
				t.Fatalf("timestamp changed: %v != %v", actual, stamp)
			}
		}
		var expires value.Timestamp
		if err = b.QueryRow(ctx, `SELECT expires_at FROM auth_sessions WHERE id=?`, UUIDBytes(session)).Scan(&expires); err != nil || expires.Micros != micros+1 || !expires.Valid {
			t.Fatal(expires, err)
		}
	}
}
