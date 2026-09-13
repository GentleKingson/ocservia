package mysql

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func TestRealVersionFourteenOwnerUpgrade(t *testing.T) {
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
	err = b.migrateChainOn(ctx, conn, chain[:13], "")
	unlock := releaseMigrationConnection(conn, lock)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	receipts := func() []string {
		t.Helper()
		var result []string
		for _, query := range []string{
			`SELECT CONCAT_WS('|',version,parent_checksum,manifest_checksum,state,repair_count,started_at,verified_at) FROM backend_schema_revisions WHERE version<=14 ORDER BY version`,
			`SELECT CONCAT_WS('|',version,ordinal,name,checksum,state,started_at,verified_at) FROM backend_schema_revision_steps WHERE version<=14 ORDER BY version,ordinal`,
		} {
			rows, err := b.Query(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var row string
				if err := rows.Scan(&row); err != nil {
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
	node, instance, connection := uuid.New(), uuid.New(), uuid.New()
	start := time.Date(1000, 1, 1, 0, 0, 0, 1000, time.UTC)
	end := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	if _, err := b.Exec(ctx, `INSERT INTO connection_owner_fencing(node_id,owner_instance_id,owner_incarnation,connection_id,owner_epoch,lease_until,updated_at)VALUES(?,?,8,?,42,?,?)`, node[:], UUIDBytes(instance), connection[:], end, start); err != nil {
		t.Fatal(err)
	}
	if err := b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, receipts()) {
		t.Fatal("published v1-v14 receipts changed")
	}
	var gotInstance uuid.UUID
	var gotConnection []byte
	var incarnation, epoch int64
	var until, updated value.Timestamp
	if err := b.QueryRow(ctx, `SELECT owner_instance_id,owner_incarnation,connection_id,owner_epoch,lease_until,updated_at FROM connection_owner_fencing WHERE node_id=?`, node[:]).Scan(&gotInstance, &incarnation, &gotConnection, &epoch, &until, &updated); err != nil || gotInstance != instance || incarnation != 8 || !bytes.Equal(gotConnection, connection[:]) || epoch != 42 || until != fixtureTimestamp(t, end) || updated != fixtureTimestamp(t, start) {
		t.Fatal("owner migration changed term or timestamp", gotInstance, incarnation, gotConnection, epoch, until, updated, err)
	}
	if err := b.ValidateSchema(ctx, 35); err != nil {
		t.Fatal(err)
	}
}
