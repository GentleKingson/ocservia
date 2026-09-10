package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRealLocalSliceLifecycle(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	if err := owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = cfg.FormatDSN()
	backend, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	s := localslice.NewBackend(backend, nil)
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	trace := "00-" + strings.Repeat("1", 32) + "-" + strings.Repeat("2", 16) + "-01"
	create := func() localslice.Operation {
		t.Helper()
		v, err := s.Create(ctx, localslice.Scenario{}, uuid.NewString(), trace)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	first, second := create(), create()
	firstID, secondID := uuid.MustParse(first.ID), uuid.MustParse(second.ID)
	firstNode, secondNode := uuid.MustParse(*first.NodeID), uuid.MustParse(*second.NodeID)
	var workspace uuid.UUID
	var workspaces int
	if err := owner.QueryRow(ctx, `SELECT id,(SELECT count(*) FROM workspaces WHERE BINARY slug=BINARY 'local-simulator') FROM workspaces WHERE BINARY slug=BINARY 'local-simulator'`).Scan(&workspace, &workspaces); err != nil || workspaces != 1 {
		t.Fatal("workspace reuse", workspace, workspaces, err)
	}
	positive, negative := value.Timestamp{Valid: true, Micros: value.PositiveInfinity}, value.Timestamp{Valid: true, Micros: value.NegativeInfinity}
	run(`UPDATE local_slice_jobs SET available_at=?,expires_at=?`, negative, positive)
	// A held job is excluded without blocking or consuming its claim window.
	locked, err := backend.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback(context.Background())
	var lockedID uuid.UUID
	if err := locked.QueryRow(ctx, `SELECT operation_id FROM local_slice_jobs WHERE operation_id=? FOR UPDATE`, UUIDBytes(firstID)).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ClaimJobs(ctx, 2)
	if err != nil || len(jobs) != 1 || jobs[0].OperationID != secondID {
		t.Fatal("skip locked claim", jobs, err)
	}
	if err := locked.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDispatchStarted(ctx, secondID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDispatchError(ctx, secondID, "retry"); err != nil {
		t.Fatal(err)
	}
	var dispatched, available value.Timestamp
	var attempts int
	var lastError string
	if err := owner.QueryRow(ctx, `SELECT dispatched_at,available_at,attempts,last_error FROM local_slice_jobs WHERE operation_id=?`, UUIDBytes(secondID)).Scan(&dispatched, &available, &attempts, &lastError); err != nil || dispatched.Valid || !available.Valid || attempts != 1 || lastError != "retry" {
		t.Fatal("retry job", dispatched, available, attempts, lastError, err)
	}
	if err := s.MarkDispatchStarted(ctx, secondID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDispatchStarted(ctx, secondID); err == nil {
		t.Fatal("duplicate dispatch accepted")
	}
	jobs, err = s.ClaimJobs(ctx, 2)
	if err != nil || len(jobs) != 1 || jobs[0].OperationID != firstID {
		t.Fatal("remaining claim", jobs, err)
	}
	if err := s.MarkDispatchStarted(ctx, firstID); err != nil {
		t.Fatal(err)
	}
	ingest := func(node uuid.UUID) uuid.UUID {
		t.Helper()
		event := uuid.Must(uuid.NewV7())
		endpoint := sha256.Sum256(append([]byte("ocservia/development-simulator/"), node[:]...))
		if err := s.Ingest(ctx, &transportv1.TransportEvent{EventId: event[:], NodeId: node[:], EndpointId: endpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT, OccurredAt: timestamppb.Now(), Traceparent: trace, Payload: []byte("heartbeat")}); err != nil {
			t.Fatal(err)
		}
		return event
	}
	firstEvent, secondEvent := ingest(firstNode), ingest(secondNode)
	page, more, err := s.ListEventsInWorkspace(ctx, workspace, uuid.Nil, 1, localslice.ListEventsDescending)
	if err != nil || !more || len(page) != 1 || page[0].ID != secondEvent.String() {
		t.Fatal("event descending page", page, more, err)
	}
	page, more, err = s.ListEventsInWorkspace(ctx, workspace, secondEvent, 1, localslice.ListEventsDescending)
	if err != nil || more || len(page) != 1 || page[0].ID != firstEvent.String() {
		t.Fatal("event descending cursor", page, more, err)
	}
	if _, found, err := s.EventSequenceInWorkspace(ctx, uuid.New(), firstEvent); err != nil || found {
		t.Fatal("cross-workspace cursor", found, err)
	}
	if sequence, found, err := s.EventSequenceInWorkspace(ctx, workspace, firstEvent); err != nil || !found || sequence != page[0].Sequence {
		t.Fatal("event sequence", sequence, found, err)
	}
	before, err := s.LastEventID(ctx)
	if err != nil || !bytes.Equal(before, secondEvent[:]) {
		t.Fatal("durable cursor", before, err)
	}
	registryFailure := errors.New("isolated registry failure")
	if err := s.ReconcileEventGap(ctx, func(context.Context, []byte) (bool, error) { return false, registryFailure }); !errors.Is(err, registryFailure) {
		t.Fatal("registry failure", err)
	}
	after, err := s.LastEventID(ctx)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("registry failure changed cursor", after, err)
	}
	connected := func(_ context.Context, node []byte) (bool, error) { return bytes.Equal(node, secondNode[:]), nil }
	run(`CREATE TRIGGER local_gap_test_failure BEFORE INSERT ON transport_events FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='isolated gap rollback'`)
	err = s.ReconcileEventGap(ctx, connected)
	run(`DROP TRIGGER local_gap_test_failure`)
	if err == nil {
		t.Fatal("injected gap failure committed")
	}
	after, err = s.LastEventID(ctx)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("gap failure advanced cursor", after, err)
	}
	state, err := s.GetOperation(ctx, firstID)
	if err != nil || state.State != "running" {
		t.Fatal("gap failure changed operation", state, err)
	}
	if err := s.ReconcileEventGap(ctx, connected); err != nil {
		t.Fatal(err)
	}
	after, err = s.LastEventID(ctx)
	if err != nil || after != nil {
		t.Fatal("gap retained durable cursor", after, err)
	}
	for _, id := range []uuid.UUID{firstID, secondID} {
		v, err := s.GetOperation(ctx, id)
		if err != nil || v.State != "unknown" {
			t.Fatal("dispatched gap outcome", v, err)
		}
	}
	var status string
	if err := owner.QueryRow(ctx, `SELECT status FROM nodes WHERE id=?`, UUIDBytes(firstNode)).Scan(&status); err != nil || status != "offline" {
		t.Fatal("disconnected gap node", status, err)
	}
	if err := owner.QueryRow(ctx, `SELECT status FROM nodes WHERE id=?`, UUIDBytes(secondNode)).Scan(&status); err != nil || status != "active" {
		t.Fatal("connected gap node", status, err)
	}
	page, _, err = s.ListEvents(ctx, secondEvent, 10)
	if err != nil || len(page) != 1 || page[0].Type != "disconnected" {
		t.Fatal("synthetic disconnect not in history", page, err)
	}
	// Occurrence time does not determine journal order. Extended values remain
	// exact through readers and JSON while the received-at default stays finite.
	historical := uuid.Must(uuid.NewV7())
	run(`INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload)VALUES(?,?,'heartbeat',?,?,'historical')`, UUIDBytes(historical), UUIDBytes(firstNode), negative, trace)
	page, _, err = s.ListEventsInWorkspace(ctx, workspace, uuid.Nil, 1, localslice.ListEventsDescending)
	if err != nil || len(page) != 1 || page[0].OccurredAt != negative {
		t.Fatal("logical history", page, err)
	}
	encoded, err := json.Marshal(page[0])
	if err != nil || !bytes.Contains(encoded, []byte(`"occurred_at":"-infinity"`)) {
		t.Fatal("logical event JSON", string(encoded), err)
	}
	var received value.Timestamp
	if err := owner.QueryRow(ctx, `SELECT received_at FROM transport_events WHERE event_id=?`, UUIDBytes(historical)).Scan(&received); err != nil || !received.Valid || received.Micros == value.PositiveInfinity {
		t.Fatal("received clock default", received, err)
	}
	third := create()
	thirdID := uuid.MustParse(third.ID)
	run(`UPDATE local_slice_jobs SET expires_at=? WHERE operation_id=?`, negative, UUIDBytes(thirdID))
	if err := s.ExpireJobs(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := s.GetOperation(ctx, thirdID)
	if err != nil || v.State != "expired" {
		t.Fatal("infinite expiry", v, err)
	}
	// A failed intent insertion must not leave a node, operation or audit row.
	var countBefore, countAfter int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM nodes`).Scan(&countBefore); err != nil {
		t.Fatal(err)
	}
	run(`CREATE TRIGGER local_create_test_failure BEFORE INSERT ON local_slice_jobs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='isolated creation rollback'`)
	_, err = s.Create(ctx, localslice.Scenario{}, uuid.NewString(), trace)
	run(`DROP TRIGGER local_create_test_failure`)
	if err == nil {
		t.Fatal("injected creation failure committed")
	}
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM nodes`).Scan(&countAfter); err != nil || countAfter != countBefore {
		t.Fatal("partial creation", countBefore, countAfter, err)
	}
	if err := owner.ValidateSchema(ctx, 34); err != nil {
		t.Fatal(err)
	}
}
