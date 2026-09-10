package mysql

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func TestRealVersionFourDataUpgrade(t *testing.T) {
	b, _ := versionTwoFixture(t, false)
	ctx := context.Background()
	chain, err := loadRevisionChain(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	err = b.migrateChainOn(ctx, conn, chain[:3], "")
	unlock := releaseMigrationConnection(conn, lock)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	receipts := func() []string {
		t.Helper()
		var result []string
		for _, query := range []string{
			`SELECT CONCAT_WS('|',version,parent_checksum,manifest_checksum,state,repair_count,started_at,verified_at) FROM backend_schema_revisions WHERE version<=4 ORDER BY version`,
			`SELECT CONCAT_WS('|',version,ordinal,name,checksum,state,started_at,verified_at) FROM backend_schema_revision_steps WHERE version<=4 ORDER BY version,ordinal`,
		} {
			rows, err := b.Query(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var row string
				if err = rows.Scan(&row); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				result = append(result, row)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	before := receipts()
	id, session := UUIDBytes(uuid.New()), UUIDBytes(uuid.New())
	old := time.Date(1000, 1, 1, 0, 0, 0, 123456000, time.UTC)
	if _, err = b.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES(?,'v4-history','finite',?,?)`, id, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES(?,?,?,?)`, session, id, old.Add(time.Hour), old); err != nil {
		t.Fatal(err)
	}
	if err = b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, receipts()) {
		t.Fatal("published v4 history rewritten")
	}
	var created, updated, disabled, expires, revoked value.Timestamp
	if err = b.QueryRow(ctx, `SELECT i.created_at,i.updated_at,i.disabled_at,s.expires_at,s.revoked_at FROM identities i JOIN auth_sessions s ON s.identity_id=i.id WHERE i.id=?`, id).Scan(&created, &updated, &disabled, &expires, &revoked); err != nil {
		t.Fatal(err)
	}
	if created != fixtureTimestamp(t, old) || updated != created || disabled.Valid || revoked.Valid || expires != fixtureTimestamp(t, old.Add(time.Hour)) {
		t.Fatalf("v4 finite/NULL values changed: %v %v %v %v %v", created, updated, disabled, expires, revoked)
	}
	if err = b.ValidateSchema(ctx, 34); err != nil {
		t.Fatal(err)
	}
}
