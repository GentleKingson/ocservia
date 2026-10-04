package operations_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

// Only fixture issuance uses a historical database clock. Retention itself
// reads the genuine server clock and runs with restricted runtime privileges.
type historicalAuditStore struct {
	auditstore.Store
	at time.Time
}

func (s historicalAuditStore) Clock(context.Context) (time.Time, error) { return s.at, nil }

type historicalAuditTx struct {
	database.Tx
	at time.Time
}

func (t historicalAuditTx) AuditStore() auditstore.Store {
	return historicalAuditStore{t.Tx.(auditstore.Provider).AuditStore(), t.at}
}

func TestAuditRetentionBackendIntegration(t *testing.T) {
	f := newOutboxFixture(t)
	ctx := context.Background()
	manager := audit.NewBackendManager(f.backend, bytes.Repeat([]byte{42}, 32))
	old := time.Now().UTC().Add(-400 * 24 * time.Hour)
	ids := []uuid.UUID{}
	for i := 0; i < 36; i++ {
		at := old.Add(time.Duration(i) * time.Microsecond)
		if i == 35 {
			at = time.Now().UTC()
		}
		id := uuid.Must(uuid.NewV7())
		ids = append(ids, id)
		if err := database.Within(ctx, f.backend, database.ReadCommitted, func(tx database.Tx) error {
			return audit.AppendChainTx(ctx, historicalAuditTx{tx, at}, audit.ChainRecord{EventID: id, WorkspaceID: f.workspace, ActorType: "user", ActorID: "operator", Action: "rebind.test", ResourceType: "node", ResourceID: id, RequestID: id.String(), Reason: "aged detail", BeforeSummary: json.RawMessage(`{"old_detail":true}`)})
		}); err != nil {
			t.Fatal(err)
		}
	}
	node := f.node(t)
	oldStamp, _ := value.FromTime(old)
	nowStamp, _ := value.FromTime(time.Now().UTC())
	for _, at := range []value.Timestamp{oldStamp, nowStamp} {
		f.exec(t, `INSERT INTO telemetry_security_events(event_id,node_id,observed_at,severity,event_type,detail)VALUES($1,$2,$3,'warning','retention.test','{"detail":true}')`, `INSERT INTO telemetry_security_events(event_id,node_id,observed_at,severity,event_type,detail)VALUES(?,?,?,'warning','retention.test','{"detail":true}')`, uuid.New(), node, at)
	}
	if err := manager.CheckpointAll(ctx); err != nil {
		t.Fatal(err)
	}
	assertChain := func(compacted int64) {
		t.Helper()
		v, err := manager.Verify(ctx, f.workspace)
		if err != nil || !v.Valid || !v.Checkpoint || v.Events != 36 || v.CompactedEvents != compacted {
			t.Fatal("audit verification", v, err)
		}
	}
	assertChain(0)
	if err := manager.CompactDetails(coordination.WithFence(ctx, lostRetentionFence{}), 365); !errors.Is(err, coordination.ErrLeadershipLost) {
		t.Fatal("fence", err)
	}
	assertChain(0)
	if err := manager.CompactDetails(ctx, 365); err != nil {
		t.Fatal(err)
	}
	assertChain(32)
	var originalMAC []byte
	q, args := f.query(`SELECT event_mac FROM audit_events WHERE id=$1`, `SELECT event_mac FROM audit_events WHERE id=?`, []any{ids[34]})
	if err := f.owner.QueryRow(ctx, q, args...).Scan(&originalMAC); err != nil {
		t.Fatal(err)
	}
	f.replaceAuditEvidence(t, ids[34], "event_mac", bytes.Repeat([]byte{0}, 32))
	if err := manager.CompactDetails(ctx, 365); err == nil {
		t.Fatal("corrupt original record compacted")
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE details_compacted_at IS NOT NULL`, `SELECT count(*) FROM audit_events WHERE details_compacted_at IS NOT NULL`); n != 32 {
		t.Fatal("corrupt batch partially committed")
	}
	f.replaceAuditEvidence(t, ids[34], "event_mac", originalMAC)
	assertChain(32)
	if err := manager.CompactDetails(ctx, 365); err != nil {
		t.Fatal(err)
	}
	assertChain(35)
	if err := manager.CompactDetails(ctx, 365); err != nil {
		t.Fatal(err)
	}
	assertChain(35)
	if n := f.count(t, `SELECT count(*) FROM telemetry_security_events WHERE details_compacted_at IS NOT NULL AND octet_length(detail_sha256)=32`, `SELECT count(*) FROM telemetry_security_events WHERE details_compacted_at IS NOT NULL AND OCTET_LENGTH(detail_sha256)=32`); n != 1 {
		t.Fatal("security cutoff", n)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE reason='aged detail'`, `SELECT count(*) FROM audit_events WHERE BINARY reason=BINARY 'aged detail'`); n != 1 {
		t.Fatal("fresh audit detail removed", n)
	}
	// Normal runtime writes cannot bypass the narrow database procedure.
	for _, queries := range [][2]string{{`UPDATE audit_events SET event_hash=event_hash`, `UPDATE audit_events SET event_hash=event_hash`}, {`DELETE FROM audit_events`, `DELETE FROM audit_events`}, {`TRUNCATE audit_events`, `TRUNCATE audit_events`}} {
		q, _ := f.query(queries[0], queries[1], nil)
		if _, err := f.backend.Exec(ctx, q); err == nil {
			t.Fatal("direct audit mutation accepted", q)
		}
	}
	// Simulate owner-level database corruption. SQL ownership cannot forge the
	// application MAC, even though original hashes and checkpoints are intact.
	f.replaceAuditEvidence(t, ids[0], "compaction_mac", bytes.Repeat([]byte{0}, 32))
	v, err := manager.Verify(ctx, f.workspace)
	if err != nil || v.Valid {
		t.Fatal("compacted metadata tampering accepted", v, err)
	}
}
func (f *outboxFixture) replaceAuditEvidence(t *testing.T, id uuid.UUID, field string, data []byte) {
	if field != "event_mac" && field != "compaction_mac" {
		t.Fatal("invalid test evidence field")
	}
	if !f.mysql {
		f.exec(t, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only`, ``)
		defer f.exec(t, `ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only`, ``)
	} else {
		var restore string
		var name, mode, charset, collation, databaseCollation, created any
		if err := f.owner.QueryRow(context.Background(), `SHOW CREATE TRIGGER audit_events_reject_update`).Scan(&name, &mode, &restore, &charset, &collation, &databaseCollation, &created); err != nil {
			t.Fatal(err)
		}
		f.exec(t, ``, `DROP TRIGGER audit_events_reject_update`)
		defer f.exec(t, ``, restore)
	}
	f.exec(t, `UPDATE audit_events SET `+field+`=$1 WHERE id=$2`, `UPDATE audit_events SET `+field+`=? WHERE id=?`, data, id)
}
