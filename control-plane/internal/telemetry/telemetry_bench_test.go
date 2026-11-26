package telemetry

import (
	"fmt"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func buildBenchWirePayload(b *testing.B, sessions, users, groups, samples int) (uuid.UUID, []byte, time.Time) {
	b.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	batchID, nodeID, instID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	wire := &agentv1.TelemetryBatch{
		BatchId:  batchID[:],
		NodeId:   nodeID[:],
		Sequence: 42,
		Priority: agentv1.TelemetryPriority_TELEMETRY_PRIORITY_CURRENT_HEALTH,
		Snapshot: &agentv1.ObservedSnapshot{
			ObservedAt:      timestamppb.New(now),
			BootId:          "boot-bench-01",
			AgentInstanceId: instID[:],
			AgentVersion:    "1.2.0",
			OcservVersion:   "1.4.2",
			OsRelease:       "debian-12",
			Architecture:    "amd64",
			OcservJson:      []byte(`{"active_state":"active","sub_state":"running","workers":4}`),
			SystemJson:      []byte(`{"memory_used_bytes":134217728,"cpu_percent":12.5}`),
			PathJson:        []byte(`{"mode":"direct","rtt_ms":14}`),
		},
		Sessions: make([]*agentv1.SessionObservation, 0, sessions),
		Users:    make([]*agentv1.UserObservation, 0, users),
		Groups:   make([]*agentv1.GroupObservation, 0, groups),
		Samples:  make([]*agentv1.MetricSample, 0, samples),
	}
	for i := range sessions {
		wire.Sessions = append(wire.Sessions, &agentv1.SessionObservation{
			SessionId:   fmt.Sprintf("sess-%06d", i),
			Username:    fmt.Sprintf("user%04d", i%max(1, users)),
			ClientIp:    fmt.Sprintf("192.0.2.%d", (i%250)+1),
			ConnectedAt: timestamppb.New(now.Add(-time.Duration(i) * time.Second)),
			BytesIn:     uint64(1024 * (i + 1)),
			BytesOut:    uint64(2048 * (i + 1)),
		})
	}
	for i := range users {
		wire.Users = append(wire.Users, &agentv1.UserObservation{
			Username: fmt.Sprintf("user%04d", i),
			Enabled:  true,
			Revision: uint64(i + 1),
		})
	}
	for i := range groups {
		members := make([]string, 0, 8)
		for m := range 8 {
			if m < users {
				members = append(members, fmt.Sprintf("user%04d", m))
			}
		}
		wire.Groups = append(wire.Groups, &agentv1.GroupObservation{
			GroupName: fmt.Sprintf("group%03d", i),
			Members:   members,
			Revision:  uint64(i + 1),
		})
	}
	metrics := []string{"session_count", "connection_rtt_ms", "cpu_usage_ratio", "memory_used_bytes", "network_rx_bytes", "network_tx_bytes"}
	for i := range samples {
		wire.Samples = append(wire.Samples, &agentv1.MetricSample{
			SampledAt: timestamppb.New(now.Add(-time.Duration(i%60) * time.Second)),
			Metric:    metrics[i%len(metrics)],
			Value:     float64(i) + 0.5,
		})
	}
	payload, err := proto.Marshal(wire)
	if err != nil {
		b.Fatal(err)
	}
	return nodeID, payload, now
}

func BenchmarkValidateWireTelemetry(b *testing.B) {
	nodeID, payload, now := buildBenchWirePayload(b, 500, 100, 30, 200)
	svc := &Service{now: func() time.Time { return now }}

	b.Run("DecodeAndValidateWire_500Sessions", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(payload)))
		for b.Loop() {
			if _, _, err := svc.validateWire(nodeID, payload); err != nil {
				b.Fatal(err)
			}
		}
	})

	batch, err := decodeWire(payload)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("ValidateBatchOnly_500Sessions", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := validateBatch(batch, now); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkGetNodeActions(b *testing.B) {
	pool, _ := testfixture.OpenIsolated(b, b, "bench_node")
	ctx := b.Context()
	workspace := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Benchmark','benchmark',now(),now())`, workspace); err != nil {
		b.Fatal(err)
	}
	svc := New(postgres.WrapPool(pool))
	for _, status := range []string{"active", "revoked"} {
		b.Run(status, func(b *testing.B) {
			id := uuid.Must(uuid.NewV7())
			if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,$3,now(),now())`, id, workspace, status); err != nil {
				b.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.certificate.issue',true),($1,'ocserv.users.write',false)`, id); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				node, err := svc.GetNode(ctx, id)
				if err != nil || node.EffectiveActions["certificate.issue"].Allowed != (status == "active") || node.EffectiveActions["user.manage"].Allowed {
					b.Fatalf("unexpected node actions: %v, %v", node.EffectiveActions, err)
				}
			}
		})
	}
}
