package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func telemetryBackend(t *testing.T) database.Backend {
	t.Helper()
	return testfixture.OpenRuntime(t)
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
	batch.Users = []User{{Username: "alice", Enabled: true, Revision: 1}}
	batch.Groups = []Group{{Name: "operators", Members: []string{"alice"}, Revision: 1}}
	batch.Security = []SecurityEvent{
		{ID: uuid.Must(uuid.NewV7()), ObservedAt: now, Severity: "info", Type: "test.ingest", Detail: json.RawMessage(`{"large":1e1000}`)},
		{ID: uuid.Must(uuid.NewV7()), ObservedAt: now, Severity: "warning", Type: "test.warning", Detail: json.RawMessage(`{"count":2}`)},
	}
	batch.Snapshot.System = json.RawMessage(`{"precise":123456789012345678901234567890,"large":1e1000}`)
	if inserted, err := service.Ingest(ctx, batch); err != nil || !inserted {
		t.Fatalf("ingest: %v %v", inserted, err)
	}
	if inserted, err := service.Ingest(ctx, batch); err != nil || inserted {
		t.Fatalf("duplicate: %v %v", inserted, err)
	}
	var storedJSON json.RawMessage
	var storedTime time.Time
	if err := backend.QueryRow(ctx, `SELECT system,observed_at FROM node_observed_snapshots WHERE node_id=$1`, node).Scan(&storedJSON, &storedTime); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedJSON, batch.Snapshot.System) && len(storedJSON) == 0 || !storedTime.Equal(batch.Snapshot.ObservedAt) {
		t.Fatalf("snapshot values changed: %s %v", storedJSON, storedTime)
	}
	read, err := service.GetNode(ctx, node)
	if err != nil || read.ID != node.String() || read.ObservedAt == nil || !read.ObservedAt.Equal(batch.Snapshot.ObservedAt) || len(read.System) == 0 || read.SessionCount != 1 {
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
	if err != nil || more || len(sessionPage) != 1 || !sessionPage[0].ConnectedAt.Equal(batch.Sessions[0].ConnectedAt) || sessionPage[0].ClientIP != batch.Sessions[0].ClientIP {
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
		rows, err := tx.Query(ctx, `SELECT group_name FROM observed_groups WHERE node_id=$1 ORDER BY group_name COLLATE "C"`, node)
		if err != nil {
			return err
		}
		defer rows.Close()
		var groupNames []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			groupNames = append(groupNames, name)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(groupNames) != 1 || groupNames[0] != "operators" {
			t.Fatalf("groups: %+v", groupNames)
		}
		for _, event := range batch.Security {
			var detail json.RawMessage
			if err := tx.QueryRow(ctx, `SELECT detail FROM telemetry_security_events WHERE event_id=$1`, event.ID).Scan(&detail); err != nil {
				return err
			}
			if len(detail) == 0 {
				t.Fatalf("security detail missing")
			}
		}
		points, err := store.History(ctx, node, "connection_rtt_ms", "raw", now.Add(-time.Hour))
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
	var total, closed int
	if err := backend.QueryRow(ctx, `SELECT count(*), count(absent_at) FROM vpn_session_history WHERE node_id=$1`, node).Scan(&total, &closed); err != nil || total != 2 || closed != 1 {
		t.Fatalf("session history after retry, stale snapshot and id reuse: total=%d closed=%d err=%v", total, closed, err)
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
	service.now = func() time.Time { return now.Add(5 * time.Minute) }
	checkState := func(status string, events int) {
		t.Helper()
		var got string
		var updated time.Time
		if err := backend.QueryRow(ctx, `SELECT status,updated_at FROM nodes WHERE id=$1`, node).Scan(&got, &updated); err != nil || got != status {
			t.Fatalf("maintenance state: %s %v", got, err)
		}
		if status == "offline" && !updated.Equal(service.now()) {
			t.Fatal("maintenance node clock", updated, service.now())
		}
		if err := backend.QueryRow(ctx, `SELECT count(*) FROM transport_events WHERE node_id=$1 AND event_type='disconnected'`, node).Scan(&count); err != nil || count != events {
			t.Fatalf("maintenance events: %d %v", count, err)
		}
	}
	checkState("active", 0)
	for range 2 {
		if err := service.Maintain(ctx); err != nil {
			t.Fatal(err)
		}
		checkState("offline", 1)
		for _, resolution := range []string{"5m", "1h"} {
			points, err := service.History(ctx, node, "connection_rtt_ms", resolution, now.Add(-time.Hour))
			if err != nil || len(points) == 0 {
				t.Fatalf("service rollup %s: %+v %v", resolution, points, err)
			}
		}
	}
}
