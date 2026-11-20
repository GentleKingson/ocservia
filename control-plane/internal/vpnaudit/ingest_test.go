package vpnaudit_test

import (
	"context"
	agent "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	wire "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/GentleKingson/ocservia/control-plane/internal/vpnaudit"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestVpnAuditCommitAndReplay(t *testing.T) {
	pool := testfixture.OpenRuntimePool(t)
	ctx := context.Background()
	ws := testfixture.InsertWorkspace(t, ctx, pool, "Audit", "vpn")
	node, ep := testfixture.InsertActiveNode(t, ctx, pool, ws, "audit")
	conn := uuid.New()
	testutil.Exec(t, ctx, pool, `INSERT INTO connection_owner_fencing(node_id,owner_instance_id,owner_incarnation,connection_id,owner_epoch,lease_until) VALUES($1,$2,1,$3,1,now()+interval '1 hour')`, node[:], uuid.New(), conn[:])
	at := time.Now().UnixMilli()
	usage := uint64(1048576)
	start := &agent.VpnAuditRecord{EventId: uuid.NewString(), EventType: "connect", SessionId: "42", Username: "alice", ClientIp: "192.0.2.4", VpnIp: "10.0.1.2", ConnectedMs: at, ObservedMs: at}
	flow := &agent.VpnAuditRecord{EventId: uuid.NewString(), EventType: "flow", SessionId: "42", Username: "alice", ClientIp: "192.0.2.4", VpnIp: "10.0.1.2", ConnectedMs: at, ObservedMs: at, FlowStartedMs: at, DestinationIp: "203.0.113.2", Protocol: "udp", BytesOut: &usage}
	request := &wire.StoreVpnAuditRequest{NodeId: node[:], EndpointId: ep[:], ConnectionId: conn[:], OwnerEpoch: 1, Batch: &agent.VpnAuditBatch{Records: []*agent.VpnAuditRecord{start, flow}}}
	backend := postgres.WrapPool(pool)
	for range 2 {
		receipt, err := vpnaudit.Ingest(ctx, backend, request)
		if err != nil || len(receipt.GetEventIds()) != 2 {
			t.Fatalf("commit: %v %v", receipt, err)
		}
	}
	var sessions, flows int
	testutil.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM vpn_session_events WHERE node_id=$1),(SELECT count(*) FROM vpn_network_history WHERE node_id=$1 AND bytes_out=$2)`, node, usage).Scan(&sessions, &flows))
	if sessions != 1 || flows != 1 {
		t.Fatal(sessions, flows)
	}
	// Session evidence at 31 days survives a cleanup that expires a flow of the same age.
	aged := time.Now().Add(-31 * 24 * time.Hour).UnixMilli()
	start.EventId, flow.EventId = uuid.NewString(), uuid.NewString()
	start.ConnectedMs, start.ObservedMs = aged, aged
	flow.ConnectedMs, flow.FlowStartedMs, flow.ObservedMs = aged, aged, aged
	_, err := vpnaudit.Ingest(ctx, backend, request)
	testutil.NoError(t, err)
	testutil.Exec(t, ctx, pool, `SELECT prune_vpn_history()`)
	testutil.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM vpn_session_events WHERE node_id=$1),(SELECT count(*) FROM vpn_network_history WHERE node_id=$1)`, node).Scan(&sessions, &flows))
	if sessions != 2 || flows != 1 {
		t.Fatalf("independent retention failed: %d, %d", sessions, flows)
	}
	// A bad member cannot publish the valid prefix of a batch.
	start.EventId = uuid.NewString()
	flow.DestinationIp = "not-an-address"
	if ack, err := vpnaudit.Ingest(ctx, backend, request); err == nil || ack != nil {
		t.Fatal("invalid audit batch acknowledged")
	}
	var exists bool
	testutil.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM vpn_session_events WHERE node_id=$1 AND event_id=$2)`, node, start.EventId).Scan(&exists))
	if exists {
		t.Fatal("partial batch committed")
	}
	flow.DestinationIp = "203.0.113.12"
	request.EndpointId = make([]byte, 32)
	if ack, err := vpnaudit.Ingest(ctx, backend, request); err == nil || ack != nil {
		t.Fatal("foreign endpoint acknowledged")
	}
	request.EndpointId = ep[:]
	request.OwnerEpoch = 2
	if _, err := vpnaudit.Ingest(ctx, backend, request); err == nil {
		t.Fatal("expired fence accepted")
	}
}

func BenchmarkVpnAuditBatch(b *testing.B) {
	pool := testfixture.OpenRuntimePool(b)
	ctx := context.Background()
	workspace := testfixture.InsertWorkspace(b, ctx, pool, "Audit benchmark", "audit-bench")
	node, endpoint := testfixture.InsertActiveNode(b, ctx, pool, workspace, "flow-bench")
	conn := uuid.New()
	testutil.Exec(b, ctx, pool, `INSERT INTO connection_owner_fencing(node_id,owner_instance_id,owner_incarnation,connection_id,owner_epoch,lease_until) VALUES($1,$2,1,$3,1,now()+interval '1 hour')`, node[:], uuid.New(), conn[:])
	backend := postgres.WrapPool(pool)
	at := time.Now().UnixMilli()
	request := &wire.StoreVpnAuditRequest{NodeId: node[:], EndpointId: endpoint[:], ConnectionId: conn[:], OwnerEpoch: 1, Batch: &agent.VpnAuditBatch{}}
	for range 32 {
		request.Batch.Records = append(request.Batch.Records, &agent.VpnAuditRecord{EventType: "flow", SessionId: "1", Username: "bench", ClientIp: "192.0.2.1", VpnIp: "10.0.0.3", ConnectedMs: at, ObservedMs: at, FlowStartedMs: at, DestinationIp: "203.0.113.12", Protocol: "udp"})
	}
	for b.Loop() {
		for _, r := range request.Batch.Records {
			r.EventId = uuid.NewString()
		}
		if _, err := vpnaudit.Ingest(ctx, backend, request); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(32, "records/batch")
}
