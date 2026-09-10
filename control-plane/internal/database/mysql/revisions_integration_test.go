package mysql

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func historicalFixture(t *testing.T, old bool) (*Backend, Options) {
	t.Helper()
	b, _, o := fixture(t)
	m, sum, err := loadManifest(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	if old {
		data, err := manifests.ReadFile("history/f6cd0e0/" + string(b.engine) + ".json")
		if err != nil {
			t.Fatal(err)
		}
		m, sum, err = decodeManifest(b.engine, data)
		if err != nil {
			t.Fatal(err)
		}
	}
	conn, name, err := migrationConnection(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	err = b.migrateBaseline(context.Background(), conn, m, sum, "")
	unlock := releaseMigrationConnection(conn, name)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	return b, o
}

func baselineReceipts(t *testing.T, b *Backend) []string {
	t.Helper()
	result := []string{}
	for _, query := range []string{
		`SELECT CONCAT_WS('|',singleton,engine,manifest_checksum,version,dirty,controller_schema,minimum_controller_schema,repair_count,updated_at) FROM backend_migrations ORDER BY singleton`,
		`SELECT CONCAT_WS('|',ordinal,name,checksum,state,started_at,verified_at) FROM backend_migration_steps ORDER BY ordinal`,
	} {
		rows, err := b.Query(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var value string
			if err = rows.Scan(&value); err != nil {
				t.Fatal(err)
			}
			result = append(result, value)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func TestRealAppendOnlyDraftUpgrade(t *testing.T) {
	for _, old := range []bool{true, false} {
		name := "144b660"
		if old {
			name = "f6cd0e0_and_6a6e3c5"
		}
		t.Run(name, func(t *testing.T) {
			b, _ := historicalFixture(t, old)
			ctx := context.Background()
			before := baselineReceipts(t, b)
			id := UUIDBytes(uuid.New())
			now := time.Now().UTC()
			if _, err := b.Exec(ctx, `INSERT INTO identities(id,issuer,subject,email,created_at,updated_at) VALUES(?,?,?,?,?,?)`, id, "Issuer ", "Subject", nil, now, now); err != nil {
				t.Fatal(err)
			}
			if err := b.ValidateSchema(ctx, 34); err == nil {
				t.Fatal("old baseline accepted as latest revision")
			}
			results := make(chan error, 2)
			for range 2 {
				go func() { results <- b.Migrate(ctx, "") }()
			}
			for range 2 {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Migrate(ctx, ""); err != nil {
				t.Fatal(err)
			}
			if err := b.ValidateSchema(ctx, 34); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, baselineReceipts(t, b)) {
				t.Fatal("baseline receipts rewritten")
			}
			var issuer, subject string
			var email *string
			if err := b.QueryRow(ctx, `SELECT issuer,subject,email FROM identities WHERE id=?`, id).Scan(&issuer, &subject, &email); err != nil || issuer != "Issuer " || subject != "Subject" || email != nil {
				t.Fatal("existing data changed", err)
			}
			var version, count int
			if err := b.QueryRow(ctx, `SELECT MAX(version) FROM backend_schema_revisions WHERE state='verified'`).Scan(&version); err != nil || version != latestRevisionVersion {
				t.Fatal("missing appended version", err)
			}
			if err := b.QueryRow(ctx, `SELECT count(*) FROM backend_schema_revision_steps WHERE version=2`).Scan(&count); err != nil || old && count == 0 || !old && count != 0 {
				t.Fatal("invented or missing ALTER receipts", count, err)
			}
			if _, err := b.Exec(ctx, `UPDATE identities SET email=? WHERE id=?`, "bad\x00value", id); !errors.Is(err, database.ErrConstraint) {
				t.Fatal("upgrade did not enforce text semantics", err)
			}
			var maximum int
			if err := b.QueryRow(ctx, `SELECT CHARACTER_MAXIMUM_LENGTH FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='secret_provider_refs' AND COLUMN_NAME='key_path'`).Scan(&maximum); err != nil || maximum != 513 {
				t.Fatal("length upgrade missing", err)
			}
			if _, err := b.Exec(ctx, `UPDATE backend_schema_revisions SET manifest_checksum=REPEAT('0',64)`); err != nil {
				t.Fatal(err)
			}
			sum, _ := ManifestChecksum(b.engine)
			if err := b.Migrate(ctx, sum); !errors.Is(err, ErrChecksum) {
				t.Fatal("repair rewrote revision checksum", err)
			}
		})
	}
}

func TestRealRevisionConstraintFailureAndRepair(t *testing.T) {
	b, _ := historicalFixture(t, true)
	ctx := context.Background()
	before := baselineReceipts(t, b)
	id := UUIDBytes(uuid.New())
	now := time.Now().UTC()
	if _, err := b.Exec(ctx, `INSERT INTO identities(id,issuer,subject,email,created_at,updated_at) VALUES(?,?,?,?,?,?)`, id, "issuer", "subject", "bad\x00value", now, now); err != nil {
		t.Fatal(err)
	}
	if err := b.Migrate(ctx, ""); !errors.Is(err, database.ErrConstraint) {
		t.Fatal("incompatible historical data not rejected", err)
	}
	if err := b.Migrate(ctx, ""); !errors.Is(err, ErrDirty) {
		t.Fatal("failed revision not dirty", err)
	}
	if err := b.ValidateSchema(ctx, 34); !errors.Is(err, ErrDirty) {
		t.Fatal("startup accepted dirty revision", err)
	}
	if !reflect.DeepEqual(before, baselineReceipts(t, b)) {
		t.Fatal("failed upgrade rewrote baseline")
	}
	// Owner explicitly corrects the invalid legacy value. Repair does not
	// sanitize application data or delete the failed migration's evidence.
	if _, err := b.Exec(ctx, `UPDATE identities SET email=NULL WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := b.Migrate(ctx, "wrong"); !errors.Is(err, ErrChecksum) {
		t.Fatal(err)
	}
	sum, _ := ManifestChecksum(b.engine)
	if err := b.Migrate(ctx, sum); err != nil {
		t.Fatal(err)
	}
	if err := b.ValidateSchema(ctx, 34); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, baselineReceipts(t, b)) {
		t.Fatal("repair rewrote baseline")
	}
}

func TestRealRevisionCrashAfterDDL(t *testing.T) {
	b, o := historicalFixture(t, true)
	ctx := context.Background()
	before := baselineReceipts(t, b)
	r, _, err := loadRevision(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	var parent string
	if err = b.QueryRow(ctx, `SELECT manifest_checksum FROM backend_migrations`).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	first := r.Parents[parent].Steps[0]
	config, err := driver.ParseDSN(o.DSN)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newFinalizeProxy(t, config.Addr, first.SQL, true)
	config.Addr = proxy.listener.Addr().String()
	child := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.timeout=90s")
	child.Env = append(os.Environ(), "PR02_CRASH_CHILD=yes", "PR02_DSN="+config.FormatDSN())
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	select {
	case <-proxy.hit:
	case <-time.After(20 * time.Second):
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatal("child did not reach committed ALTER")
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	proxy.close()
	conn, err := b.pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := schemaHash(ctx, conn, step{Name: first.Name, Kind: "table"})
	conn.Close()
	if err != nil || actual != first.After {
		t.Fatal("DDL did not commit before process death", err)
	}
	var state string
	if err = b.QueryRow(ctx, `SELECT state FROM backend_schema_revision_steps WHERE version=2 AND ordinal=1`).Scan(&state); err != nil || state != "running" {
		t.Fatal("step incorrectly marked complete", err)
	}
	if err = b.Migrate(ctx, ""); !errors.Is(err, ErrDirty) {
		t.Fatal(err)
	}
	sum, _ := ManifestChecksum(b.engine)
	if err = b.Migrate(ctx, sum); err != nil {
		t.Fatal(err)
	}
	if err = b.ValidateSchema(ctx, 34); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, baselineReceipts(t, b)) {
		t.Fatal("crash recovery rewrote baseline")
	}
	if _, err = b.Exec(ctx, `UPDATE backend_schema_revision_steps SET checksum=? WHERE version=2 AND ordinal=1`, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if err = b.Migrate(ctx, sum); !errors.Is(err, ErrChecksum) {
		t.Fatal("step conflict adopted", err)
	}
}
