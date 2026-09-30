package operations_test

import (
	"bytes"
	"context"
	"errors"
	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/historyretention"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"
)

type lostRetentionFence struct{}

func (lostRetentionFence) AssertTransaction(context.Context, database.Tx) error {
	return coordination.ErrLeadershipLost
}

func TestHistoryRetentionBackendIntegration(t *testing.T) {
	f := newOutboxFixture(t)
	ctx := context.Background()
	service := operations.NewBackend(f.backend, 1, f.signer)
	old := time.Now().UTC().Add(-100 * 24 * time.Hour)
	created, _ := value.FromTime(old.Add(-time.Hour))
	expired, _ := value.FromTime(old)
	terminal, _ := value.FromTime(old.Add(time.Minute))
	node := f.node(t)
	request := outboxRequest(node)
	var firstID string
	for i := 0; i < 37; i++ {
		next := request
		if i > 0 {
			next.IdempotencyKey = uuid.NewString()
		}
		op, _, err := service.CreateSynthetic(ctx, next)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstID = op.ID
		}
		var id uuid.UUID
		var raw []byte
		q, args := f.query(`SELECT id,envelope FROM commands WHERE operation_id=$1`, `SELECT id,envelope FROM commands WHERE operation_id=?`, []any{uuid.MustParse(op.ID)})
		if err := f.owner.QueryRow(ctx, q, args...).Scan(&id, &raw); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope.IssuedAt = timestamppb.New(old.Add(-time.Hour))
		envelope.ExpiresAt = timestamppb.New(old)
		if err := f.signer.Authorize(&envelope); err != nil {
			t.Fatal(err)
		}
		raw, err = proto.Marshal(&envelope)
		if err != nil {
			t.Fatal(err)
		}
		state := "succeeded"
		if i == 33 {
			state = "unknown"
		}
		if i == 34 {
			state = "running"
		}
		f.exec(t, `UPDATE commands SET state=$1,envelope=$2,created_at=$3,expires_at=$4,updated_at=$5 WHERE id=$6`, `UPDATE commands SET state=?,envelope=?,created_at=?,expires_at=?,updated_at=? WHERE id=?`, state, raw, created, expired, terminal, id)
		f.exec(t, `UPDATE operations SET state=$1,created_at=$2,expires_at=$3,updated_at=$4,completed_at=$5 WHERE id=$6`, `UPDATE operations SET state=?,created_at=?,expires_at=?,updated_at=?,completed_at=? WHERE id=?`, state, created, expired, terminal, terminal, uuid.MustParse(op.ID))
		f.exec(t, `UPDATE outbox_events SET payload=$1,published_at=$2 WHERE command_id=$3`, `UPDATE outbox_events SET payload=?,published_at=? WHERE command_id=?`, raw, terminal, id)
		if i == 35 {
			var outbox uuid.UUID
			q, args := f.query(`SELECT id FROM outbox_events WHERE command_id=$1`, `SELECT id FROM outbox_events WHERE command_id=?`, []any{id})
			if err := f.owner.QueryRow(ctx, q, args...).Scan(&outbox); err != nil {
				t.Fatal(err)
			}
			f.exec(t, `INSERT INTO command_attempts(id,command_id,outbox_event_id,worker_id,attempt_number,state,started_at,finished_at)VALUES($1,$2,$3,$4,1,'unknown',$5,$6)`, `INSERT INTO command_attempts(id,command_id,outbox_event_id,worker_id,attempt_number,state,started_at,finished_at)VALUES(?,?,?,?,1,'unknown',?,?)`, uuid.New(), id, outbox, uuid.New(), created, terminal)
		}
		if i == 36 {
			f.exec(t, `INSERT INTO node_command_leases(node_id,command_id,lease_token,worker_id,leased_until,created_at)VALUES($1,$2,$3,$4,$5,$6)`, `INSERT INTO node_command_leases(node_id,command_id,lease_token,worker_id,leased_until,created_at)VALUES(?,?,?,?,?,?)`, node, id, uuid.New(), uuid.New(), expired, created)
		}

	}
	worker, err := historyretention.New(f.backend, historyretention.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(coordination.WithFence(ctx, lostRetentionFence{})); !errors.Is(err, coordination.ErrLeadershipLost) {
		t.Fatal("fence", err)
	}
	count := func() int {
		return f.count(t, `SELECT count(*) FROM commands WHERE details_compacted_at IS NOT NULL`, `SELECT count(*) FROM commands WHERE details_compacted_at IS NOT NULL`)
	}
	if count() != 0 {
		t.Fatal("lost fence committed compaction")
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if count() != historyretention.BatchSize {
		t.Fatal("batch not bounded", count())
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if count() != 33 {
		t.Fatal("Unknown/running not preserved", count())
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if count() != 33 {
		t.Fatal("compaction not idempotent")
	}
	again, replay, err := service.CreateSynthetic(ctx, request)
	if err != nil || !replay || again.ID != firstID {
		t.Fatal("idempotency identity lost", again, replay, err)
	}
	q, args := f.query(`SELECT envelope,envelope_sha256 FROM commands WHERE operation_id=$1`, `SELECT envelope,envelope_sha256 FROM commands WHERE operation_id=?`, []any{uuid.MustParse(firstID)})
	var compact, digest []byte
	if err := f.owner.QueryRow(ctx, q, args...).Scan(&compact, &digest); err != nil {
		t.Fatal(err)
	}
	var header agentv1.CommandEnvelope
	if err := proto.Unmarshal(compact, &header); err != nil {
		t.Fatal(err)
	}
	if header.Payload != nil || header.Authorization == nil || len(digest) != 32 || !bytes.Equal(header.NodeId, node[:]) {
		t.Fatal("compact authority evidence missing")
	} // Ordinary revoked-node history is independent of command detail. Keep
	// identity/revocation rows, and keep snapshots needed by unresolved work.
	retiredNode, activeNode := f.node(t), f.node(t)
	for _, id := range []uuid.UUID{retiredNode, node} {
		f.exec(t, `UPDATE nodes SET status='revoked' WHERE id=$1`, `UPDATE nodes SET status='revoked' WHERE id=?`, id)
		f.exec(t, `UPDATE node_endpoint_keys SET state='revoked',revoked_at=$1 WHERE node_id=$2`, `UPDATE node_endpoint_keys SET state='revoked',revoked_at=? WHERE node_id=?`, terminal, id)
	}
	for _, id := range []uuid.UUID{retiredNode, activeNode, node} {
		f.exec(t, `INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,ocserv,system,path,last_heartbeat_at) VALUES($1,$2,'boot',$3,'test','test','test','{"detail":1}','{}','{}',$4)`, "INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,ocserv,`system`,path,last_heartbeat_at) VALUES(?,?,'boot',?,'test','test','test','{\"detail\":1}','{}','{}',?)", id, terminal, uuid.New(), terminal)
	}

	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM node_observed_snapshots WHERE node_id=$1 AND ocserv='{}'`, `SELECT count(*) FROM node_observed_snapshots WHERE node_id=? AND BINARY ocserv=BINARY '{}'`, retiredNode); n != 1 {
		t.Fatal("retired snapshot not compacted")
	}
	if n := f.count(t, `SELECT count(*) FROM node_observed_snapshots WHERE ocserv<>'{}'`, `SELECT count(*) FROM node_observed_snapshots WHERE BINARY ocserv<>BINARY '{}'`); n != 2 {
		t.Fatal("active or unresolved snapshot lost", n)
	}
	if n := f.count(t, `SELECT count(*) FROM node_endpoint_keys WHERE state='revoked'`, `SELECT count(*) FROM node_endpoint_keys WHERE state='revoked'`); n != 2 {
		t.Fatal("revocation tombstone lost")
	}

}
