package mysql

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/schemaartifact"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

const (
	logicalNowSQL = "SELECT TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))"
	logicalDay    = int64(24 * time.Hour / time.Microsecond)
)

// assertMaintenanceClock proves the audit/security maintenance routines and the
// audit trigger measure retention on the application's 2000-01-01 logical clock.
// Fresh evidence must survive caller-chosen timestamps; aged evidence must still
// be maintainable. The owner must already have granted the runtime account.
func assertMaintenanceClock(t *testing.T, owner *Backend, options Options) {
	t.Helper()
	ctx := context.Background()
	clock := func() int64 {
		t.Helper()
		var now int64
		if err := owner.QueryRow(ctx, logicalNowSQL).Scan(&now); err != nil {
			t.Fatal(err)
		}
		return now
	}
	run := func(query string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	now := clock()
	workspace, node := uuid.New(), uuid.New()
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,?,?,?,?)`, UUIDBytes(workspace), "clock", "clock-"+workspace.String(), now, now)
	run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'clock-node','active',?,?)`, UUIDBytes(node), UUIDBytes(workspace), now, now)
	mac := make([]byte, 32)
	auditEvent := func(occurred int64) (uuid.UUID, []byte) {
		t.Helper()
		id := uuid.New()
		hash := append(id[:], id[:]...)
		run(`INSERT INTO audit_events(id,workspace_id,actor_type,actor_id,action,resource_type,request_id,result,reason,event_hash,auth_version,event_key_id,event_mac,occurred_at) VALUES(?,?,'controller','clock','clock.test','workspace',?,'succeeded','detail',?,1,'clock-key',?,?)`, UUIDBytes(id), UUIDBytes(workspace), id.String(), hash, mac, occurred)
		return id, hash
	}
	securityEvent := func(observed int64) uuid.UUID {
		t.Helper()
		id := uuid.New()
		run(`INSERT INTO telemetry_security_events(event_id,node_id,observed_at,severity,event_type,detail) VALUES(?,?,?,'info','clock',?)`, UUIDBytes(id), UUIDBytes(node), observed, []byte(`{"detail":"kept"}`))
		return id
	}
	compacted := func(table, key string, id uuid.UUID) (at value.Timestamp) {
		t.Helper()
		if err := owner.QueryRow(ctx, "SELECT details_compacted_at FROM "+table+" WHERE "+key+"=?", UUIDBytes(id)).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	freshAudit, freshHash := auditEvent(now)
	youngAudit, youngHash := auditEvent(now - 60*logicalDay)
	agedAudit, agedHash := auditEvent(now - 100*logicalDay)
	staleAudit, staleHash := auditEvent(now - 100*logicalDay)

	cfg, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = cfg.FormatDSN()
	runtime, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	store := runtime.AuditStore()
	compact := func(id uuid.UUID, hash []byte, at int64) error {
		return store.CompactDetails(ctx, id, hash, "clock-key", mac, value.Timestamp{Micros: at, Valid: true})
	}

	// The defect accepted these because UNIX_TIMESTAMP is about 30 years ahead of
	// the logical clock: a caller could age fresh evidence by naming a later time.
	for name, attempt := range map[string]struct {
		id   uuid.UUID
		hash []byte
		at   int64
	}{
		"fresh event, aged by caller time": {freshAudit, freshHash, now + 91*logicalDay},
		"young event, aged by caller time": {youngAudit, youngHash, now + 31*logicalDay},
		"aged event, future caller time":   {staleAudit, staleHash, clock() + logicalDay},
	} {
		if err := compact(attempt.id, attempt.hash, attempt.at); !errors.Is(err, database.ErrNotFound) {
			t.Fatalf("%s: runtime compacted unexpired evidence: %v", name, err)
		}
	}
	for _, id := range []uuid.UUID{freshAudit, youngAudit, staleAudit} {
		if compacted("audit_events", "id", id).Valid {
			t.Fatal("rejected compaction changed audit evidence")
		}
	}
	at := clock()
	if err := compact(agedAudit, agedHash, at); err != nil {
		t.Fatal("aged audit detail not maintainable", err)
	}
	if got := compacted("audit_events", "id", agedAudit); !got.Valid || got.Micros != at {
		t.Fatal("audit compaction time not the logical clock", got, at)
	}

	// The audit trigger is the last line of defence against direct owner-side
	// updates; it must use the same clock and the same 90-day floor.
	trigger := func(id uuid.UUID, compactedAt int64) error {
		_, err := owner.Exec(ctx, `UPDATE audit_events SET reason=NULL,before_summary=NULL,after_summary=NULL,details_compacted_at=?,compaction_key_id='clock-key',compaction_mac=? WHERE id=?`, compactedAt, mac, UUIDBytes(id))
		return err
	}
	for name, attempt := range map[string]struct {
		id uuid.UUID
		at int64
	}{
		"fresh event":        {freshAudit, now + 91*logicalDay},
		"young event":        {youngAudit, now + 31*logicalDay},
		"aged, future clock": {staleAudit, clock() + logicalDay},
	} {
		// 1644 is the trigger's SIGNAL; the backend redacts the message text.
		if err := trigger(attempt.id, attempt.at); err == nil || !strings.Contains(err.Error(), "server error 1644") {
			t.Fatalf("%s: trigger accepted unexpired compaction: %v", name, err)
		}
	}
	if err := trigger(staleAudit, clock()); err != nil {
		t.Fatal("trigger rejected aged compaction", err)
	}

	freshSecurity := securityEvent(now)
	youngSecurity := securityEvent(now - 60*logicalDay)
	agedSecurity := securityEvent(now - 100*logicalDay)
	before := clock()
	// A far-future cutoff may never shorten the retention floor.
	if err := store.CompactSecurity(ctx, value.Timestamp{Micros: now + 1000*logicalDay, Valid: true}); err != nil {
		t.Fatal(err)
	}
	after := clock()
	for _, id := range []uuid.UUID{freshSecurity, youngSecurity} {
		if compacted("telemetry_security_events", "event_id", id).Valid {
			t.Fatal("runtime compacted security detail inside retention")
		}
	}
	if got := compacted("telemetry_security_events", "event_id", agedSecurity); !got.Valid || got.Micros < before || got.Micros > after {
		t.Fatal("security compaction time not the logical clock", got, before, after)
	}
}

// revisionOneArtifacts reconstructs the last schema/upgrade pair before the
// logical-clock revision by reversing exactly that revision's three object
// bodies. The pinned revision-1 checkpoint proves the bytes are the original
// fresh-install file; any later revision must retire or replace this helper.
func revisionOneArtifacts(t *testing.T) (schema, upgrade []byte) {
	t.Helper()
	current, err := loadMySQLArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(current.upgrade.Revisions) != 2 {
		t.Fatal("revision-1 reconstruction is only valid while revision 2 is the head")
	}
	currentSchema, _ := artifactSources.ReadFile("mysql/schema.sql")
	currentUpgrade, _ := artifactSources.ReadFile("mysql/upgrade.sql")
	upgradeText, _, found := strings.Cut(string(currentUpgrade), "-- ocservia:revision=2\n")
	if !found {
		t.Fatal("revision 2 missing")
	}
	text := string(currentSchema)
	for _, create := range current.upgrade.Revisions[1].Steps {
		meta := current.meta(create)
		if meta.Kind == "data" || meta.Before != "" {
			continue
		}
		var previous string
		for _, drop := range current.upgrade.Revisions[1].Steps {
			if m := current.meta(drop); m.After == "" && m.Kind == meta.Kind && m.Object == meta.Object {
				previous = m.Before
			}
		}
		block := regexp.MustCompile("(?s)-- ocservia:step=[0-9]+:" + regexp.QuoteMeta(strings.SplitN(create.Name, ":", 2)[1]) + "\n.*?-- ocservia:end-step\n")
		original := block.FindString(text)
		if original == "" || previous == "" || strings.Count(text, original) != 1 {
			t.Fatal("cannot locate schema step", create.Name)
		}
		reverted := strings.NewReplacer(
			`"after":"`+meta.After+`"`, `"after":"`+previous+`"`,
			"TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))", "CAST(UNIX_TIMESTAMP(UTC_TIMESTAMP(6))*1000000 AS SIGNED)",
		).Replace(original)
		text = strings.Replace(text, original, reverted, 1)
	}
	parsed, err := schemaartifact.Parse([]byte(upgradeText), "mysql")
	if err != nil {
		t.Fatal(err)
	}
	history, err := schemaartifact.HistoryChecksum(parsed, 1)
	if err != nil {
		t.Fatal(err)
	}
	text = strings.Replace(text, "-- ocservia:revision=2\n", "-- ocservia:revision=1\n", 1)
	text = regexp.MustCompile(`(?m)^-- ocservia:history-sha256=.*$`).ReplaceAllString(text, "-- ocservia:history-sha256="+history)
	if digest([]byte(text)) != current.upgrade.Revisions[0].Checkpoint.Checksum {
		t.Fatal("reconstructed revision-1 schema differs from its pinned checkpoint")
	}
	return []byte(text), []byte(upgradeText)
}

func TestMySQLForwardRevisionMaintenanceClock(t *testing.T) {
	ctx := context.Background()
	currentSchema, _ := artifactSources.ReadFile("mysql/schema.sql")
	currentUpgrade, _ := artifactSources.ReadFile("mysql/upgrade.sql")
	schemaV1, upgradeV1 := revisionOneArtifacts(t)

	// A genuine fresh revision-1 installation, produced by the real initializer.
	old, ownerAdmin, options := fixture(t)
	useMySQLArtifacts(t, schemaV1, upgradeV1)
	if err := old.Migrate(ctx, ""); err != nil {
		t.Fatal("fresh revision 1", err)
	}
	prior := baselineReceipts(t, old)
	if len(prior) != 1 || !strings.HasPrefix(prior[0], "2:1:") {
		t.Fatal("unexpected fresh revision-1 receipts", prior)
	}

	useMySQLArtifacts(t, currentSchema, currentUpgrade)
	for range 2 {
		if err := old.Migrate(ctx, ""); err != nil {
			t.Fatal("forward upgrade from revision 1", err)
		}
	}
	// The routines were recreated, so their EXECUTE grants are gone until the
	// ordinary post-migration runtime authorization runs again.
	if err := old.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	if err := old.ValidateSchema(ctx); err != nil {
		t.Fatal(err)
	}
	after := baselineReceipts(t, old)
	head, err := ArtifactChecksum(MySQL, "upgrade", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 || after[0] != prior[0] || !strings.HasPrefix(after[1], "2:2:"+head+":verified:") {
		t.Fatal("receipts rewritten or revision 2 missing", prior, after)
	}

	fresh, _, _ := migrateFixture(t)
	if err := fresh.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	if cutoverBusinessState(t, old) != cutoverBusinessState(t, fresh) {
		t.Fatal("fresh and forward-upgraded schema/seeds differ")
	}
	if cutoverGrants(t, ownerAdmin, old) != cutoverGrants(t, ownerAdmin, fresh) {
		t.Fatal("fresh and forward-upgraded runtime ACL differ")
	}
	assertMaintenanceClock(t, old, options)
}

func TestMySQLFreshMaintenanceClock(t *testing.T) {
	b, _, options := migrateFixture(t)
	if err := b.GrantTestPrivileges(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertMaintenanceClock(t, b, options)
	var count int
	if err := b.QueryRow(context.Background(), "SELECT COUNT(*) FROM schema_revisions WHERE epoch=2 AND revision=2 AND state='verified'").Scan(&count); err != nil || count != 1 {
		t.Fatal(fmt.Sprint("fresh revision-2 receipt: ", count, err))
	}
}
