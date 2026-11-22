package operations_test

import (
	"bytes"
	"context"
	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/historyretention"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"
)

func TestHistoryRetentionBackendIntegration(t *testing.T) {
	f := newOutboxFixture(t)
	ctx := context.Background()
	service := operations.NewBackend(f.backend, 1, f.signer)
	old := time.Now().UTC().Add(-100 * 24 * time.Hour)
	created := old.Add(-time.Hour)
	expired := old
	terminal := old.Add(time.Minute)
	node := f.node(t)
	request := outboxRequest(node)
	var firstID string
	for i := 0; i < 37; i++ {
		next := request
		if i > 0 {
			next.IdempotencyKey = uuid.NewString()
		}
		op, _, err := service.Create(ctx, next)
		testutil.NoError(t, err)
		if i == 0 {
			firstID = op.ID
		}
		var id uuid.UUID
		var raw []byte
		testutil.NoError(t, f.owner.QueryRow(ctx, `SELECT id,envelope FROM commands WHERE operation_id=$1`, uuid.MustParse(op.ID)).Scan(&id, &raw))
		var envelope agentv1.CommandEnvelope
		testutil.NoError(t, proto.Unmarshal(raw, &envelope))
		envelope.IssuedAt = timestamppb.New(old.Add(-time.Hour))
		envelope.ExpiresAt = timestamppb.New(old)
		testutil.NoError(t, f.signer.Authorize(&envelope))
		raw, err = proto.Marshal(&envelope)
		testutil.NoError(t, err)
		state := "succeeded"
		if i == 33 {
			state = "unknown"
		}
		if i == 34 {
			state = "running"
		}
		f.exec(t, `UPDATE commands SET state=$1,envelope=$2,created_at=$3,expires_at=$4,updated_at=$5 WHERE id=$6`, state, raw, created, expired, terminal, id)
		f.exec(t, `UPDATE operations SET state=$1,created_at=$2,expires_at=$3,updated_at=$4,completed_at=$5 WHERE id=$6`, state, created, expired, terminal, terminal, uuid.MustParse(op.ID))
		f.exec(t, `UPDATE outbox_events SET payload=$1,published_at=$2 WHERE command_id=$3`, raw, terminal, id)
		if i == 35 {
			var outbox uuid.UUID
			testutil.NoError(t, f.owner.QueryRow(ctx, `SELECT id FROM outbox_events WHERE command_id=$1`, id).Scan(&outbox))
			f.exec(t, `INSERT INTO command_attempts(id,command_id,outbox_event_id,worker_id,attempt_number,state,started_at,finished_at)VALUES($1,$2,$3,$4,1,'unknown',$5,$6)`, uuid.New(), id, outbox, uuid.New(), created, terminal)
		}
		if i == 36 {
			f.exec(t, `INSERT INTO node_command_leases(node_id,command_id,lease_token,worker_id,leased_until,created_at)VALUES($1,$2,$3,$4,$5,$6)`, node, id, uuid.New(), uuid.New(), expired, created)
		}

	}
	worker, err := historyretention.New(f.backend, historyretention.DefaultPolicy())
	testutil.NoError(t, err)
	count := func() int {
		return f.count(t, `SELECT count(*) FROM commands WHERE details_compacted_at IS NOT NULL`)
	}
	testutil.NoError(t, worker.RunOnce(ctx))
	if count() != historyretention.BatchSize {
		t.Fatal("batch not bounded", count())
	}
	testutil.NoError(t, worker.RunOnce(ctx))
	if count() != 33 {
		t.Fatal("Unknown/running not preserved", count())
	}
	testutil.NoError(t, worker.RunOnce(ctx))
	if count() != 33 {
		t.Fatal("compaction not idempotent")
	}
	again, replay, err := service.Create(ctx, request)
	if err != nil || !replay || again.ID != firstID {
		t.Fatal("idempotency identity lost", again, replay, err)
	}
	var compact []byte
	testutil.NoError(t, f.owner.QueryRow(ctx, `SELECT envelope FROM commands WHERE operation_id=$1`, uuid.MustParse(firstID)).Scan(&compact))
	var header agentv1.CommandEnvelope
	testutil.NoError(t, proto.Unmarshal(compact, &header))
	if header.Payload != nil || header.Authorization == nil || !bytes.Equal(header.NodeId, node[:]) {
		t.Fatal("compact authority evidence missing")
	} // Ordinary revoked-node history is independent of command detail. Keep
	// identity/revocation rows, and keep snapshots needed by unresolved work.
	retiredNode, activeNode := f.node(t), f.node(t)
	for _, id := range []uuid.UUID{retiredNode, node} {
		f.exec(t, `UPDATE nodes SET status='revoked' WHERE id=$1`, id)
		f.exec(t, `UPDATE node_endpoint_keys SET state='revoked',revoked_at=$1 WHERE node_id=$2`, terminal, id)
	}
	for _, id := range []uuid.UUID{retiredNode, activeNode, node} {
		f.exec(t, `INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,ocserv,system,path,last_heartbeat_at) VALUES($1,$2,'boot',$3,'test','test','test','{"detail":1}','{}','{}',$4)`, id, terminal, uuid.New(), terminal)
	}

	testutil.NoError(t, worker.RunOnce(ctx))
	if n := f.count(t, `SELECT count(*) FROM node_observed_snapshots WHERE node_id=$1 AND ocserv='{}'`, retiredNode); n != 1 {
		t.Fatal("retired snapshot not compacted")
	}
	if n := f.count(t, `SELECT count(*) FROM node_observed_snapshots WHERE ocserv<>'{}'`); n != 2 {
		t.Fatal("active or unresolved snapshot lost", n)
	}
	if n := f.count(t, `SELECT count(*) FROM node_endpoint_keys WHERE state='revoked'`); n != 2 {
		t.Fatal("revocation tombstone lost")
	}

}
