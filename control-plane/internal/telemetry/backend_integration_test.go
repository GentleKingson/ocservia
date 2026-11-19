package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func telemetryBackend(t *testing.T) database.Backend {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("real PostgreSQL required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	testutil.NoError(t, err)
	t.Cleanup(pool.Close)
	return postgres.WrapPool(pool)
}

// This calls the real ingestion service with the runtime principal, including
// shared usage, observed groups/security, history and the same rollback boundary.
func TestTelemetryBackendWorkflowIntegration(t *testing.T) {
	backend := telemetryBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	node, workspace := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := backend.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Ingest',$2,now(),now())`, workspace, "ingest-"+workspace.String())
	exec(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'node','offline',now(),now())`, node, workspace)
	exec(`UPDATE nodes SET updated_at=$1 WHERE id=$2`, value.Timestamp{Valid: true, Micros: value.PositiveInfinity}, node)
	service := NewBackend(backend)
	unobserved, err := service.GetNode(ctx, node)
	if err != nil || unobserved.ObservedAt != nil || unobserved.Freshness != "never" {
		t.Fatalf("missing snapshot: %+v %v", unobserved, err)
	}
	if unobserved.EffectiveActions["user.manage"].Allowed {
		t.Fatal("readonly node advertised user writes")
	}
	exec(`INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,'ocserv.users.write',true)`, node)
	writable, err := service.GetNode(ctx, node)
	if err != nil || !writable.EffectiveActions["user.manage"].Allowed || writable.EffectiveActions["group.manage"].Allowed {
		t.Fatalf("approved action read: %+v %v", writable.EffectiveActions, err)
	}
	exec(`UPDATE node_capabilities SET approved=false WHERE node_id=$1`, node)

	now := time.Now().UTC().Truncate(time.Microsecond)
	wireID, instance := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	wire, err := proto.Marshal(&agentv1.TelemetryBatch{BatchId: wireID[:], NodeId: node[:], Sequence: 0, Priority: agentv1.TelemetryPriority_TELEMETRY_PRIORITY_CURRENT_HEALTH, Snapshot: &agentv1.ObservedSnapshot{ObservedAt: timestamppb.New(now.Add(-time.Minute)), BootId: "wire", AgentInstanceId: instance[:], AgentVersion: "0.1.0", OcservVersion: "1.3.0", OsRelease: "debian", OcservJson: []byte(`{}`), SystemJson: []byte(`{}`), PathJson: []byte(`{}`)}})
	testutil.NoError(t, err)
	outerFailure := errors.New("authoritative endpoint changed")
	err = database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		if ok, err := service.IngestWireTransaction(ctx, tx, node, wire); err != nil || !ok {
			t.Fatalf("outer wire ingest: %v %v", ok, err)
		}
		return outerFailure
	})
	if !errors.Is(err, outerFailure) {
		t.Fatal(err)
	}
	if ok, err := service.IngestWire(ctx, node, wire); err != nil || !ok {
		t.Fatalf("wire retry after outer rollback: %v %v", ok, err)
	}
	if _, err := service.IngestWire(ctx, uuid.Must(uuid.NewV7()), wire); !errors.Is(err, ErrInvalidTelemetry) {
		t.Fatalf("wire node mismatch: %v", err)
	}
	batch := testBatch(node, 1, now)
	batch.Sessions[0].ClientIP = "2001:db8::10"
	seconds := uint64(20)
	batch.IPBans = []IPBan{{IP: "2001:db8::20", SecondsRemaining: &seconds}, {IP: "192.0.2.9"}, {IP: "2001:db8::2"}}
	batch.Users = []User{{Username: "alice", Enabled: true, Revision: 1, Fingerprint: make([]byte, 32)}}
	batch.Groups = []Group{{Name: "operators", Members: []string{"alice"}, Revision: 1, Fingerprint: make([]byte, 32)}}
	batch.Security = []SecurityEvent{{ID: uuid.Must(uuid.NewV7()), ObservedAt: now, Severity: "info", Type: "test.ingest", Detail: json.RawMessage(`{"large":1e1000}`)}}
	batch.Snapshot.System = json.RawMessage(`{"precise":123456789012345678901234567890,"large":1e1000}`)
	if inserted, err := service.Ingest(ctx, batch); err != nil || !inserted {
		t.Fatalf("ingest: %v %v", inserted, err)
	}
	if inserted, err := service.Ingest(ctx, batch); err != nil || inserted {
		t.Fatalf("duplicate: %v %v", inserted, err)
	}
	var storedJSON value.JSONB
	var storedTime value.Timestamp
	if err := backend.QueryRow(ctx, `SELECT system,observed_at FROM node_observed_snapshots WHERE node_id=$1`, node).Scan(&storedJSON, &storedTime); err != nil {
		t.Fatal(err)
	}
	expectedJSON, _ := value.ParseJSONB(batch.Snapshot.System)
	expectedTime, _ := value.FromTime(batch.Snapshot.ObservedAt)
	if !bytes.Equal(storedJSON.Bytes(), expectedJSON.Bytes()) || storedTime != expectedTime {
		t.Fatalf("snapshot logical values changed: %s %+v", storedJSON.Bytes(), storedTime)
	}
	read, err := service.GetNode(ctx, node)
	if err != nil || read.ID != node.String() || read.ObservedAt == nil || *read.ObservedAt != expectedTime || !bytes.Equal(read.System, expectedJSON.Bytes()) || read.SessionCount != 1 {
		t.Fatalf("node read model: %+v %v", read, err)
	}
	page, more, err := service.ListNodesInWorkspace(ctx, workspace, uuid.Nil, 1)
	if err != nil || more || len(page) != 1 || page[0].ID != node.String() {
		t.Fatalf("node workspace page: %+v %v %v", page, more, err)
	}
	page, more, err = service.ListNodesInWorkspace(ctx, workspace, node, 1)
	if err != nil || more || len(page) != 0 {
		t.Fatalf("node cursor: %+v %v %v", page, more, err)
	}
	if _, err := service.GetNode(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("missing node: %v", err)
	}
	sessionPage, more, err := service.ListSessions(ctx, node, "", 1)
	connected, _ := value.FromTime(batch.Sessions[0].ConnectedAt)
	if err != nil || more || len(sessionPage) != 1 || sessionPage[0].ConnectedAt != connected || sessionPage[0].ClientIP != batch.Sessions[0].ClientIP {
		t.Fatalf("session read: %+v %v %v", sessionPage, more, err)
	}
	bans, err := service.ListIPBans(ctx, node, 200)
	if err != nil || len(bans) != len(batch.IPBans) {
		t.Fatalf("IP ban read: %+v %v", bans, err)
	}
	for i, want := range []IPBan{batch.IPBans[1], batch.IPBans[2], batch.IPBans[0]} {
		got := bans[i]
		if got.IP != want.IP || (got.SecondsRemaining == nil) != (want.SecondsRemaining == nil) || (got.SecondsRemaining != nil && *got.SecondsRemaining != *want.SecondsRemaining) {
			t.Fatalf("IPv4/IPv6 ordering at %d: %+v want %+v", i, bans[i], want)
		}
	}
	if next, more, err := service.ListSessions(ctx, node, sessionPage[0].ID, 1); err != nil || more || len(next) != 0 {
		t.Fatalf("session cursor repeated IPv6 session: %+v %v %v", next, more, err)
	}
	err = database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := telemetrystore.FromTransaction(tx)
		if err != nil {
			return err
		}
		groups, err := store.Groups(ctx, node)
		if err != nil {
			return err
		}
		if len(groups) != 1 || groups[0].Name != "operators" {
			t.Fatalf("groups: %+v", groups)
		}
		event, err := store.SecurityEvent(ctx, batch.Security[0].ID)
		if err != nil {
			return err
		}
		expected, _ := value.ParseJSONB(batch.Security[0].Detail)
		if !bytes.Equal(event.Detail.Bytes(), expected.Bytes()) {
			t.Fatalf("security detail lost precision: %s", event.Detail.Bytes())
		}
		since, err := value.FromTime(now.Add(-time.Hour))
		if err != nil {
			return err
		}
		points, err := store.History(ctx, node, "connection_rtt_ms", "raw", since)
		if err != nil {
			return err
		}
		if len(points) != 1 {
			t.Fatalf("history after duplicate: %+v", points)
		}
		return nil
	})
	testutil.NoError(t, err)
	// A stale snapshot still contributes history, without replacing current sessions.
	stale := testBatch(node, 2, now.Add(-time.Minute))
	stale.Sessions = nil
	if ok, err := service.Ingest(ctx, stale); !ok || err != nil {
		t.Fatalf("stale: %v %v", ok, err)
	}
	var count int
	if err := backend.QueryRow(ctx, `SELECT count(*) FROM node_sessions WHERE node_id=$1`, node).Scan(&count); err != nil || count != 1 {
		t.Fatalf("stale replaced sessions: %d %v", count, err)
	}
	// The session identity conflict occurs after the batch and snapshot writes.
	// Both must roll back, and the original batch ID must remain retryable.
	conflict := testBatch(node, 3, now.Add(time.Second))
	conflict.Sessions[0].ConnectedAt = batch.Sessions[0].ConnectedAt
	conflict.Sessions[0].Username = "bob"
	if _, err := service.Ingest(ctx, conflict); !errors.Is(err, ErrInvalidTelemetry) {
		t.Fatalf("usage conflict: %v", err)
	}
	conflict.Sessions[0].Username = "alice"
	if ok, err := service.Ingest(ctx, conflict); !ok || err != nil {
		t.Fatalf("retry rolled-back batch: %v %v", ok, err)
	}
	concurrent := testBatch(node, 6, now.Add(3*time.Second))
	type outcome struct {
		inserted bool
		err      error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() { inserted, err := service.Ingest(ctx, concurrent); results <- outcome{inserted, err} }()
	}
	insertions := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.inserted {
			insertions++
		}
	}
	if insertions != 1 {
		t.Fatalf("concurrent duplicate inserted %d times", insertions)
	}
	invalid := testBatch(node, 4, now.Add(2*time.Second))
	invalid.Snapshot.System = json.RawMessage(`{"invalid":"\u0000"}`)
	if _, err := service.Ingest(ctx, invalid); err == nil {
		t.Fatal("NUL JSON accepted")
	}
	stopped, stop := context.WithCancel(ctx)
	stop()
	if _, err := service.Ingest(stopped, testBatch(node, 5, now)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	negative := value.Timestamp{Micros: value.NegativeInfinity, Valid: true}
	positive := value.Timestamp{Micros: value.PositiveInfinity, Valid: true}
	// Infinite historical timestamps belong to owner-seeded DEFAULT data,
	// not the runtime ingestion window. Keep the service on its runtime account.
	ownerDSN := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if ownerDSN == "" {
		t.Fatal("owner connection required to seed historical telemetry")
	}
	ownerPool, err := pgxpool.New(ctx, ownerDSN)
	testutil.NoError(t, err)
	t.Cleanup(ownerPool.Close)
	historyOwner := postgres.WrapPool(ownerPool)
	for _, at := range []value.Timestamp{negative, positive} {
		if _, err := historyOwner.Exec(ctx, `INSERT INTO telemetry_samples(node_id,batch_id,sampled_at,metric,value) VALUES($1,$2,$3,'connection_rtt_ms',7)`, node, batch.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	points, err := service.HistoryFrom(ctx, node, "connection_rtt_ms", "raw", negative)
	if err != nil || len(points) < 2 || points[0].At != negative || points[len(points)-1].At != positive {
		t.Fatalf("extended service history: %+v %v", points, err)
	}
	if encoded, err := json.Marshal(points); err != nil || !bytes.Contains(encoded, []byte(`"at":"infinity"`)) || !bytes.Contains(encoded, []byte(`"at":"-infinity"`)) {
		t.Fatalf("extended history JSON: %s %v", encoded, err)
	}
	service.now = func() time.Time { return now.Add(5 * time.Minute) }
	fenced := coordination.WithFence(ctx, telemetryRejectedFence{err: outerFailure})
	if err := service.Maintain(fenced); !errors.Is(err, outerFailure) {
		t.Fatalf("maintenance fence: %v", err)
	}
	checkState := func(status string, events int) {
		t.Helper()
		var got string
		var updated value.Timestamp
		if err := backend.QueryRow(ctx, `SELECT status,updated_at FROM nodes WHERE id=$1`, node).Scan(&got, &updated); err != nil || got != status {
			t.Fatalf("maintenance state: %s %v", got, err)
		}
		want := positive
		if status == "offline" {
			want, err = value.FromTime(service.now())
			testutil.NoError(t, err)
		}
		if updated != want {
			t.Fatal("activation/maintenance node clock", updated, want)
		}
		if err := backend.QueryRow(ctx, `SELECT count(*) FROM transport_events WHERE node_id=$1 AND event_type='disconnected'`, node).Scan(&count); err != nil || count != events {
			t.Fatalf("maintenance events: %d %v", count, err)
		}
	}
	checkState("active", 0)
	if points, err := service.HistoryFrom(ctx, node, "connection_rtt_ms", "5m", positive); err != nil || len(points) != 0 {
		t.Fatalf("fence committed rollup: %+v %v", points, err)
	}
	for range 2 {
		if err := service.Maintain(ctx); err != nil {
			t.Fatal(err)
		}
		checkState("offline", 1)
		for _, resolution := range []string{"5m", "1h"} {
			points, err := service.HistoryFrom(ctx, node, "connection_rtt_ms", resolution, positive)
			if err != nil || len(points) != 1 || points[0].At != positive || points[0].Count != 1 || points[0].Average != 7 {
				t.Fatalf("infinite service rollup %s: %+v %v", resolution, points, err)
			}
		}
	}
	for _, at := range []value.Timestamp{negative, positive} {
		exec(`UPDATE node_observed_snapshots SET last_heartbeat_at=$1,observed_at=$2 WHERE node_id=$3`, at, at, node)
		read, err := service.GetNode(ctx, node)
		testutil.NoError(t, err)
		want := "stale"
		if at == positive {
			want = "fresh"
		}
		if read.ObservedAt == nil || *read.ObservedAt != at || read.Freshness != want {
			t.Fatalf("extended node: %+v", read)
		}
	}
}

type telemetryRejectedFence struct {
	coordination.Fence
	err error
}

func (f telemetryRejectedFence) AssertTransaction(context.Context, database.Tx) error { return f.err }
