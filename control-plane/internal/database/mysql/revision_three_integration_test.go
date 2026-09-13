package mysql

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
)

func versionTwoFixture(t *testing.T, old bool) (*Backend, Options) {
	t.Helper()
	b, o := historicalFixture(t, old)
	r, sum, err := loadRevision(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	conn, name, err := migrationConnection(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	err = b.migrateChainOn(context.Background(), conn, []revisionArtifact{{r, sum}}, "")
	unlock := releaseMigrationConnection(conn, name)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	return b, o
}

func versionTwoReceipts(t *testing.T, b *Backend) []string {
	t.Helper()
	result := baselineReceipts(t, b)
	for _, query := range []string{
		`SELECT CONCAT_WS('|',version,parent_checksum,manifest_checksum,state,repair_count,started_at,verified_at) FROM backend_schema_revisions WHERE version=2`,
		`SELECT CONCAT_WS('|',version,ordinal,name,checksum,state,started_at,verified_at) FROM backend_schema_revision_steps WHERE version=2 ORDER BY ordinal`,
	} {
		rows, err := b.Query(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var receipt string
			if err = rows.Scan(&receipt); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			result = append(result, receipt)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func TestRealVersionTwoReceiptsSurviveNextRevision(t *testing.T) {
	if latestRevisionVersion < 3 {
		t.Skip("next revision not yet published")
	}
	for _, old := range []bool{true, false} {
		b, _ := versionTwoFixture(t, old)
		before := versionTwoReceipts(t, b)
		if err := b.Migrate(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, versionTwoReceipts(t, b)) {
			t.Fatal("published receipts changed")
		}
		if err := b.ValidateSchema(context.Background(), 35); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRealNextRevisionCrashAfterData(t *testing.T) {
	if latestRevisionVersion < 3 {
		t.Skip("next revision not yet published")
	}
	b, o := versionTwoFixture(t, false)
	ctx := context.Background()
	before := versionTwoReceipts(t, b)
	chain, err := loadRevisionChain(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	_, parent, err := baselineFor(b.engine, "")
	if err != nil {
		t.Fatal(err)
	}
	var target revisionStep
	for _, s := range chain[1].Parents[parent].Steps {
		if s.Name == "identities_guard" {
			target = s
			break
		}
	}
	if target.SQL == "" {
		t.Fatal("missing real guard backfill")
	}
	c, err := driver.ParseDSN(o.DSN)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newFinalizeProxy(t, c.Addr, target.SQL, true)
	c.Addr = proxy.listener.Addr().String()
	child := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.timeout=120s")
	child.Env = append(os.Environ(), "PR02_CRASH_CHILD=yes", "PR02_DSN="+c.FormatDSN())
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	select {
	case <-proxy.hit:
	case <-time.After(90 * time.Second):
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatal("migration did not reach data commit")
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	proxy.close()
	var state string
	if err = b.QueryRow(ctx, `SELECT state FROM backend_schema_revision_steps WHERE version=3 AND name=?`, target.Name).Scan(&state); err != nil || state != "running" {
		t.Fatal(state, err)
	}
	if err = b.Migrate(ctx, ""); !errors.Is(err, ErrDirty) {
		t.Fatal("dirty startup accepted", err)
	}
	sum, _ := ManifestChecksum(b.engine)
	if err = b.Migrate(ctx, sum); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, versionTwoReceipts(t, b)) {
		t.Fatal("repair rewrote old receipts")
	}
	if err = b.ValidateSchema(ctx, 35); err != nil {
		t.Fatal(err)
	}
}
