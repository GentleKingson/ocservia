package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func testBatch(nodeID uuid.UUID, sequence uint64, observed time.Time) Batch {
	return Batch{
		ID: uuid.Must(uuid.NewV7()), NodeID: nodeID, Sequence: sequence, Kind: "current_health",
		Snapshot: Snapshot{ObservedAt: observed, BootID: "boot-a", AgentInstance: uuid.Must(uuid.NewV7()), AgentVersion: "0.1.0", OcservVersion: "1.3.0", OSRelease: "debian", Ocserv: json.RawMessage(`{"active_state":"active"}`), System: json.RawMessage(`{"memory_used_bytes":42}`), Path: json.RawMessage(`{"mode":"direct","rtt_ms":12}`)},
		Sessions: []Session{{ID: "session-a", Username: "alice", ClientIP: "192.0.2.1", ConnectedAt: observed.Add(-time.Minute), BytesIn: 10, BytesOut: 20}},
		IPBans:   []IPBan{{IP: "192.0.2.9"}},
		Samples:  []Sample{{SampledAt: observed, Metric: "connection_rtt_ms", Value: 12}},
	}
}

func TestValidateBatchRejectsNonCanonicalIPBan(t *testing.T) {
	now := time.Now().UTC()
	batch := testBatch(uuid.Must(uuid.NewV7()), 1, now)
	batch.IPBans[0].IP = "2001:0db8::1"
	if err := validateBatch(batch, now); err == nil {
		t.Fatal("non-canonical IP ban accepted")
	}
}

func TestValidateBatchRejectsHighCardinalityMetricNames(t *testing.T) {
	now := time.Now().UTC()
	batch := testBatch(uuid.Must(uuid.NewV7()), 1, now)
	batch.Samples[0].Metric = "session_019fc0a4"
	if err := validateBatch(batch, now); err == nil {
		t.Fatal("high-cardinality metric name accepted")
	}
}

func TestValidateBatchRejectsPostgresBigintOverflow(t *testing.T) {
	now := time.Now().UTC()
	remaining := uint64(math.MaxUint64)
	tests := []struct {
		name   string
		mutate func(*Batch)
	}{
		{name: "dropped security", mutate: func(batch *Batch) { batch.Snapshot.Dropped.Security = uint64(math.MaxInt64) + 1 }},
		{name: "dropped health", mutate: func(batch *Batch) { batch.Snapshot.Dropped.Health = uint64(math.MaxInt64) + 1 }},
		{name: "dropped aggregate", mutate: func(batch *Batch) { batch.Snapshot.Dropped.Aggregate = uint64(math.MaxInt64) + 1 }},
		{name: "dropped raw", mutate: func(batch *Batch) { batch.Snapshot.Dropped.Raw = uint64(math.MaxInt64) + 1 }},
		{name: "IP ban remaining", mutate: func(batch *Batch) { batch.IPBans[0].SecondsRemaining = &remaining }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			batch := testBatch(uuid.Must(uuid.NewV7()), 1, now)
			test.mutate(&batch)
			if err := validateBatch(batch, now); err == nil {
				t.Fatal("telemetry value exceeding PostgreSQL bigint was accepted")
			}
		})
	}
}

func TestValidateBatchRejectsTelemetryOutsideRetentionWindow(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name   string
		mutate func(*Batch)
	}{
		{name: "old snapshot", mutate: func(batch *Batch) { batch.Snapshot.ObservedAt = now.Add(-MaxTelemetryAge - time.Second) }},
		{name: "future snapshot", mutate: func(batch *Batch) { batch.Snapshot.ObservedAt = now.Add(MaxTelemetrySkew + time.Second) }},
		{name: "old sample", mutate: func(batch *Batch) { batch.Samples[0].SampledAt = now.Add(-MaxTelemetryAge - time.Second) }},
		{name: "future sample", mutate: func(batch *Batch) { batch.Samples[0].SampledAt = now.Add(MaxTelemetrySkew + time.Second) }},
		{name: "old security event", mutate: func(batch *Batch) {
			batch.Security = []SecurityEvent{{ID: uuid.Must(uuid.NewV7()), ObservedAt: now.Add(-MaxTelemetryAge - time.Second), Severity: "warning", Type: "test", Detail: json.RawMessage(`{}`)}}
		}},
		{name: "future security event", mutate: func(batch *Batch) {
			batch.Security = []SecurityEvent{{ID: uuid.Must(uuid.NewV7()), ObservedAt: now.Add(MaxTelemetrySkew + time.Second), Severity: "warning", Type: "test", Detail: json.RawMessage(`{}`)}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			batch := testBatch(uuid.Must(uuid.NewV7()), 1, now)
			test.mutate(&batch)
			if err := validateBatch(batch, now); err == nil {
				t.Fatal("telemetry outside the accepted retention window was accepted")
			}
		})
	}
}

func TestMaximumUserGroupSnapshotFitsWireAndValidation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	maximumName := func(prefix string, index int) string {
		base := fmt.Sprintf("%s%06d", prefix, index)
		return base + strings.Repeat("x", 64-len(base))
	}
	batchID := uuid.Must(uuid.NewV7())
	nodeID := uuid.Must(uuid.NewV7())
	agentInstanceID := uuid.Must(uuid.NewV7())
	wire := &agentv1.TelemetryBatch{
		BatchId: batchID[:], NodeId: nodeID[:], Sequence: 1,
		Priority: agentv1.TelemetryPriority_TELEMETRY_PRIORITY_CURRENT_HEALTH,
		Snapshot: &agentv1.ObservedSnapshot{ObservedAt: timestamppb.New(now), BootId: "boot", AgentInstanceId: agentInstanceID[:], AgentVersion: "0.1.0", OcservVersion: "1.3.0", OsRelease: "debian", OcservJson: []byte(`{}`), SystemJson: []byte(`{}`), PathJson: []byte(`{}`)},
	}
	for index := 0; index < MaxManagedResources; index++ {
		username := maximumName("u", index)
		wire.Users = append(wire.Users, &agentv1.UserObservation{Username: username, Enabled: true, Revision: 1, FingerprintSha256: make([]byte, sha256.Size)})
		wire.Groups = append(wire.Groups, &agentv1.GroupObservation{GroupName: maximumName("g", index), Members: []string{username}, Revision: 1, FingerprintSha256: make([]byte, sha256.Size)})
	}
	for index := MaxManagedResources; index < MaxReportedGroups; index++ {
		wire.Groups = append(wire.Groups, &agentv1.GroupObservation{GroupName: maximumName("g", index), Revision: 1, FingerprintSha256: make([]byte, sha256.Size)})
	}
	payload, err := proto.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > MaxBatchBytes {
		t.Fatalf("maximum supported telemetry is %d bytes", len(payload))
	}
	batch, err := decodeWire(payload)
	if err != nil {
		t.Fatalf("decode maximum telemetry: %v", err)
	}
	if err := validateBatch(batch, now); err != nil {
		t.Fatalf("validate maximum telemetry: %v", err)
	}
	encoded, err := json.Marshal(batch)
	if err != nil || len(encoded) > MaxBatchBytes {
		t.Fatalf("maximum internal telemetry is %d bytes: %v", len(encoded), err)
	}
	batch.Users = append(batch.Users, batch.Users[0])
	if err := validateBatch(batch, now); err == nil {
		t.Fatal("oversized user snapshot accepted")
	}
}

func TestIngestOrderingRollupOfflineAndRecoveryIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	workspaceID := uuid.Must(uuid.NewV7())
	nodeID := uuid.Must(uuid.NewV7())
	_, err = pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Telemetry',$2,now(),now())`, workspaceID, "telemetry-"+workspaceID.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'node','active',now(),now())`, nodeID, workspaceID); err != nil {
		t.Fatal(err)
	}
	desiredUserFingerprint := sha256.Sum256([]byte(`{"name":"alice","enabled":true}`))
	desiredGroupFingerprint := sha256.Sum256([]byte(`{"name":"staff","members":["alice"]}`))
	if _, err = pool.Exec(ctx, `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at) VALUES($1,'alice',true,1,1,$2,now(),now())`, nodeID, desiredUserFingerprint[:]); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO desired_groups(node_id,group_name,members,version,revision,fingerprint,created_at,updated_at) VALUES($1,'staff',ARRAY['alice'],1,1,$2,now(),now())`, nodeID, desiredGroupFingerprint[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM telemetry_security_events WHERE node_id=$1`, nodeID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM telemetry_ingest_batches WHERE node_id=$1`, nodeID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM nodes WHERE id=$1`, nodeID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM workspaces WHERE id=$1`, workspaceID)
	})
	now := time.Now().UTC().Truncate(time.Second)
	service := New(pool)
	service.now = func() time.Time { return now }
	current := testBatch(nodeID, 2, now)
	inserted, err := service.Ingest(ctx, current)
	if err != nil || !inserted {
		t.Fatalf("current ingest: inserted=%v err=%v", inserted, err)
	}
	inserted, err = service.Ingest(ctx, current)
	if err != nil || inserted {
		t.Fatalf("duplicate ingest: inserted=%v err=%v", inserted, err)
	}
	reconnected := testBatch(nodeID, current.Sequence, now.Add(time.Second))
	if inserted, err = service.Ingest(ctx, reconnected); err != nil || !inserted {
		t.Fatalf("reconnected agent sequence: inserted=%v err=%v", inserted, err)
	}
	delayed := testBatch(nodeID, 1, now.Add(-time.Minute))
	delayed.Snapshot.OcservVersion = "0.9.0"
	if inserted, err = service.Ingest(ctx, delayed); err != nil || !inserted {
		t.Fatalf("delayed ingest: %v %v", inserted, err)
	}
	node, err := service.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if node.OcservVersion != "1.3.0" || node.SessionCount != 1 || node.Freshness != "fresh" {
		t.Fatalf("unexpected current node: %#v", node)
	}
	bans, err := service.ListIPBans(ctx, nodeID, 200)
	if err != nil || len(bans) != 1 || bans[0].IP != "192.0.2.9" {
		t.Fatalf("unexpected current IP bans: %#v %v", bans, err)
	}
	if err := service.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	history, err := service.History(ctx, nodeID, "connection_rtt_ms", "5m", now.Add(-time.Hour))
	if err != nil || len(history) == 0 {
		t.Fatalf("rollup missing: %v %v", history, err)
	}
	service.now = func() time.Time { return now.Add(5*time.Minute - time.Second) }
	if err := service.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	node, err = service.GetNode(ctx, nodeID)
	if err != nil || node.ConnectionState != "offline" || node.Freshness != "stale" {
		t.Fatalf("offline state: %#v %v", node, err)
	}
	stale := testBatch(nodeID, 4, now.Add(-time.Minute))
	if inserted, err = service.Ingest(ctx, stale); err != nil || !inserted {
		t.Fatalf("stale ingest: %v %v", inserted, err)
	}
	node, err = service.GetNode(ctx, nodeID)
	if err != nil || node.ConnectionState != "offline" {
		t.Fatalf("stale telemetry revived node: %#v %v", node, err)
	}
	service.now = func() time.Time { return now.Add(5 * time.Minute) }
	recovered := testBatch(nodeID, 3, now.Add(5*time.Minute))
	recovered.IPBans = nil
	userFingerprint := sha256.Sum256([]byte(`{"name":"alice","enabled":true}`))
	groupFingerprint := sha256.Sum256([]byte(`{"name":"staff","members":["alice"]}`))
	recovered.Users = []User{{Username: "alice", Enabled: true, Revision: 1, Fingerprint: userFingerprint[:]}}
	recovered.Groups = []Group{{Name: "staff", Members: []string{"alice"}, Revision: 1, Fingerprint: groupFingerprint[:]}}
	if inserted, err = service.Ingest(ctx, recovered); err != nil || !inserted {
		t.Fatalf("recovery: %v %v", inserted, err)
	}
	var observedUsers, observedGroups int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM observed_users WHERE node_id=$1),(SELECT count(*) FROM observed_groups WHERE node_id=$1)`, nodeID).Scan(&observedUsers, &observedGroups); err != nil || observedUsers != 1 || observedGroups != 1 {
		t.Fatalf("user/group observations without IP bans: users=%d groups=%d err=%v", observedUsers, observedGroups, err)
	}
	var desiredObservedMatch bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM desired_users d JOIN observed_users o USING(node_id,username) WHERE d.node_id=$1 AND d.enabled=o.enabled AND d.revision=o.revision AND d.fingerprint=o.fingerprint) AND EXISTS(SELECT 1 FROM desired_groups d JOIN observed_groups o USING(node_id) WHERE d.node_id=$1 AND d.group_name=o.group_name AND d.members=o.members AND d.revision=o.revision AND d.fingerprint=o.fingerprint)`, nodeID).Scan(&desiredObservedMatch); err != nil || !desiredObservedMatch {
		t.Fatalf("desired/observed state did not reconcile after five-minute recovery: %v %v", desiredObservedMatch, err)
	}
	node, err = service.GetNode(ctx, nodeID)
	if err != nil || node.ConnectionState != "online" {
		t.Fatalf("online recovery: %#v %v", node, err)
	}
	var partitions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_inherits WHERE inhparent='telemetry_samples'::regclass`).Scan(&partitions); err != nil || partitions < 2 {
		t.Fatalf("monthly partition missing: %d %v", partitions, err)
	}
}
