package mysql

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func TestRealOperationDispatch(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	workspace := uuid.Must(uuid.NewV7())
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'dispatch',?,?,?)`, UUIDBytes(workspace), workspace.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now))
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
	signer, err := commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	service := operations.NewBackend(backend, 1, signer)
	newNode := func() uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,?,'active',?,?)`, UUIDBytes(id), UUIDBytes(workspace), id.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now))
		return id
	}
	create := func(node uuid.UUID) uuid.UUID {
		v, _, err := service.CreateSynthetic(ctx, operations.CreateRequest{NodeID: node, IdempotencyKey: uuid.NewString(), ExpectedVersion: 1, Kind: operations.SyntheticEcho, Message: "dispatch", TTL: time.Hour, RequestID: uuid.NewString(), Traceparent: "00-" + strings.Repeat("1", 32) + "-" + strings.Repeat("2", 16) + "-01"})
		if err != nil {
			t.Fatal(err)
		}
		return uuid.MustParse(v.ID)
	}
	worker := uuid.Must(uuid.NewV7())
	firstNode, secondNode := newNode(), newNode()
	first, second := create(firstNode), create(secondNode)
	// Concurrent workers must share the active-command budget and node lease.
	var wg sync.WaitGroup
	results, failures := make(chan []operations.Dispatch, 2), make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			jobs, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 10, time.Minute)
			results <- jobs
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var jobs []operations.Dispatch
	for got := range results {
		jobs = append(jobs, got...)
	}
	if len(jobs) != 1 || jobs[0].OperationID != first {
		t.Fatalf("concurrent capacity/FIFO: %+v", jobs)
	}
	d := jobs[0]
	if err := service.MarkFailed(ctx, d, errors.New("test transport outage")); err != nil {
		t.Fatal(err)
	}
	var attemptState string
	var finished value.Timestamp
	if err := owner.QueryRow(ctx, `SELECT state,finished_at FROM command_attempts WHERE id=?`, UUIDBytes(d.AttemptID)).Scan(&attemptState, &finished); err != nil || attemptState != "failed" || !finished.Valid {
		t.Fatal(attemptState, finished, err)
	}
	negative, positive := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	run(`UPDATE outbox_events SET available_at=? WHERE command_id=?`, negative, UUIDBytes(d.CommandID))
	jobs, err = service.Claim(ctx, worker, 10, time.Minute)
	if err != nil || len(jobs) != 1 || jobs[0].OperationID != first || jobs[0].AttemptID == d.AttemptID {
		t.Fatal("retry", jobs, err)
	}
	d = jobs[0]
	// Infinite lease clocks remain live rather than overflowing or expiring.
	run(`UPDATE node_command_leases SET created_at=?,leased_until=? WHERE lease_token=?`, negative, positive, UUIDBytes(d.LeaseToken))
	run(`UPDATE outbox_events SET locked_until=? WHERE id=?`, positive, UUIDBytes(d.OutboxID))
	if err := database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		if err := commandlimit.Lock(ctx, tx); err != nil {
			return err
		}
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		return store.ExtendDispatch(ctx, d, worker, time.Minute)
	}); err != nil {
		t.Fatal("extend exact claim", err)
	}
	// Closing an invalid attempt must roll back publishing and all projections.
	rejected := errors.New("rollback dispatch completion")
	err = database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		if err := store.PublishDispatch(ctx, d, positive); err != nil {
			return err
		}
		if err := store.RecordDispatched(ctx, d, d.Envelope, false, positive); err != nil {
			return err
		}
		if err := store.CloseDispatch(ctx, d, positive); err != nil {
			return err
		}
		return rejected
	})
	if !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	v, err := service.Get(ctx, first)
	if err != nil || v.State != "queued" {
		t.Fatal("completion rollback", v, err)
	}
	if err := service.MarkSent(ctx, d); err != nil {
		t.Fatal("sent", err)
	}
	v, err = service.Get(ctx, first)
	if err != nil || v.State != "dispatched" {
		t.Fatal(v, err)
	}
	if jobs, err := service.Claim(ctx, worker, 10, time.Minute); err != nil || len(jobs) != 0 {
		t.Fatal("active capacity exceeded", jobs, err)
	}
	// Unknown observation attempts can run at a full budget but must not
	// turn their unresolved logical state into dispatched again.
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(d.Envelope, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.DeliveryMode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY
	encoded, err := proto.Marshal(&envelope)
	if err != nil {
		t.Fatal(err)
	}
	run(`UPDATE commands SET state='unknown',envelope=? WHERE id=?`, encoded, UUIDBytes(d.CommandID))
	run(`UPDATE operations SET state='unknown' WHERE id=?`, UUIDBytes(first))
	run(`UPDATE outbox_events SET payload=?,published_at=NULL,available_at=? WHERE id=?`, encoded, negative, UUIDBytes(d.OutboxID))
	jobs, err = service.Claim(ctx, worker, 10, time.Minute)
	if err != nil || len(jobs) != 1 || jobs[0].OperationID != first {
		t.Fatal("unknown capacity", jobs, err)
	}
	if err := service.MarkSent(ctx, jobs[0]); err != nil {
		t.Fatal(err)
	}
	v, err = service.Get(ctx, first)
	if err != nil || v.State != "unknown" {
		t.Fatal("unknown observation dispatch", v, err)
	}
	run(`UPDATE commands SET state='succeeded' WHERE id=?`, UUIDBytes(d.CommandID))
	run(`UPDATE operations SET state='succeeded' WHERE id=?`, UUIDBytes(first))
	jobs, err = service.Claim(ctx, worker, 10, time.Minute)
	if err != nil || len(jobs) != 1 || jobs[0].OperationID != second {
		t.Fatal("released capacity", jobs, err)
	}
	if err := service.MarkSent(ctx, jobs[0]); err != nil {
		t.Fatal(err)
	}
	metrics, err := service.Metrics(ctx)
	if err != nil || metrics.Unpublished != 0 || metrics.Queued != 0 || metrics.Unknown != 0 {
		t.Fatal("queue metrics", metrics, err)
	}
	var stored []byte
	if err := owner.QueryRow(ctx, `SELECT envelope FROM commands WHERE id=?`, UUIDBytes(jobs[0].CommandID)).Scan(&stored); err != nil || !bytes.Equal(stored, jobs[0].Envelope) {
		t.Fatal("exact sent frame", err)
	}
	run(`UPDATE commands SET state='succeeded' WHERE id=?`, UUIDBytes(jobs[0].CommandID))
	run(`UPDATE operations SET state='succeeded' WHERE id=?`, UUIDBytes(second))
	node := newNode()
	third := create(node)
	jobs, err = service.Claim(ctx, worker, 10, time.Minute)
	if err != nil || len(jobs) != 1 || jobs[0].OperationID != third {
		t.Fatal("fenced claim", jobs, err)
	}
	d = jobs[0]
	if err := proto.Unmarshal(d.Envelope, &envelope); err != nil {
		t.Fatal(err)
	}
	ownerID, connectionID, fenceID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	envelope.ConnectionFence = &agentv1.ConnectionFenceV2{FenceId: fenceID[:], NodeId: node[:], OwnerInstanceId: ownerID[:], OwnerIncarnation: 1, ConnectionId: connectionID[:], OwnerEpoch: 1}
	envelope.FenceBinding = &agentv1.FenceBindingV2{FenceId: fenceID[:], NodeId: node[:], OwnerInstanceId: ownerID[:], OwnerIncarnation: 1, ConnectionId: connectionID[:], OwnerEpoch: 1, OperationId: d.CommandID[:], OperationKind: agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND}
	encoded, err = proto.Marshal(&envelope)
	if err != nil {
		t.Fatal(err)
	}
	run(`INSERT INTO connection_owner_fencing(node_id,owner_instance_id,owner_incarnation,connection_id,owner_epoch,lease_until)VALUES(?,?,1,?,2,?)`, UUIDBytes(node), UUIDBytes(ownerID), connectionID[:], fixtureTimestamp(t, now.Add(time.Hour)))
	if err := service.MarkSentWithEnvelope(ctx, d, encoded); !errors.Is(err, connectionowner.ErrNotOwner) {
		t.Fatal("stale owner accepted", err)
	}
	v, err = service.Get(ctx, third)
	if err != nil || v.State != "queued" {
		t.Fatal("stale owner changed projection", v, err)
	}
	run(`UPDATE connection_owner_fencing SET owner_epoch=1 WHERE node_id=?`, UUIDBytes(node))
	// Simulate the durable projection committed by result ingestion before a
	// delayed MarkSent. This is not a substitute for transport verification.
	resultEvent := uuid.Must(uuid.NewV7())
	resultAt := time.Now().UTC()
	run(`INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload)VALUES(?,?,'command_result',?,?,'result')`, UUIDBytes(resultEvent), UUIDBytes(node), fixtureTimestamp(t, resultAt), d.Traceparent)
	run(`INSERT INTO agent_command_results(event_id,command_id,idempotency_key,state,result,error_code,completed_at,replayed,created_at)VALUES(?,?,?,'rejected','','rejected',?,false,?)`, UUIDBytes(resultEvent), UUIDBytes(d.CommandID), UUIDBytes(uuid.Must(uuid.NewV7())), fixtureTimestamp(t, resultAt), fixtureTimestamp(t, resultAt))
	stamp := fixtureTimestamp(t, resultAt)
	run(`UPDATE commands SET state='rejected',updated_at=? WHERE id=?`, stamp, UUIDBytes(d.CommandID))
	run(`UPDATE operations SET state='failed',updated_at=? WHERE id=?`, stamp, UUIDBytes(third))
	run(`UPDATE outbox_events SET published_at=?,locked_by=NULL,locked_until=NULL WHERE id=?`, stamp, UUIDBytes(d.OutboxID))
	run(`UPDATE command_attempts SET state='sent',finished_at=? WHERE id=?`, stamp, UUIDBytes(d.AttemptID))
	run(`DELETE FROM node_command_leases WHERE lease_token=?`, UUIDBytes(d.LeaseToken))
	for range 2 {
		if err := service.MarkSentWithEnvelope(ctx, d, encoded); err != nil {
			t.Fatal("result-completed dispatch replay", err)
		}
	}
	if err := owner.QueryRow(ctx, `SELECT envelope FROM commands WHERE id=?`, UUIDBytes(d.CommandID)).Scan(&stored); err != nil || !bytes.Equal(stored, encoded) {
		t.Fatal("exact terminal frame", err)
	}
	v, err = service.Get(ctx, third)
	if err != nil || v.State != "failed" {
		t.Fatal("late dispatch changed terminal result", v, err)
	}
	sharedNode, otherNode := newNode(), newNode()
	sharedFirst := create(sharedNode)
	sharedNext := create(sharedNode)
	other := create(otherNode)
	wide := operations.NewBackend(backend, 50, signer)
	jobs, err = wide.Claim(ctx, worker, 10, time.Minute)
	if err != nil || len(jobs) != 2 {
		t.Fatal("per-node dispatch ranking", jobs, err)
	}
	seen := map[uuid.UUID]bool{}
	for _, job := range jobs {
		seen[job.OperationID] = true
	}
	if !seen[sharedFirst] || !seen[other] || seen[sharedNext] {
		t.Fatal("per-node FIFO", seen)
	}
	if more, err := wide.Claim(ctx, worker, 10, time.Minute); err != nil || len(more) != 0 {
		t.Fatal("node lease bypassed", more, err)
	}
	for _, job := range jobs {
		if err := wide.MarkSent(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	reconnectID := uuid.Must(uuid.NewV7())
	run(`INSERT INTO connection_owner_fencing(node_id,owner_instance_id,owner_incarnation,connection_id,owner_epoch,lease_until)VALUES(?,?,1,?,3,?)`, UUIDBytes(otherNode), UUIDBytes(ownerID), reconnectID[:], fixtureTimestamp(t, time.Now().UTC().Add(time.Hour)))
	reconnect := operations.OwnerReconnect{NodeID: otherNode, ConnectionID: reconnectID, OwnerEpoch: 3, ObservedAt: time.Now().UTC(), Limit: 16}
	err = database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		count, err := wide.RecoverAmbiguousDispatched(ctx, tx, reconnect)
		if err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("reconnect rollback count: %d", count)
		}
		return rejected
	})
	if !errors.Is(err, rejected) {
		t.Fatal("reconnect rollback", err)
	}
	v, err = wide.Get(ctx, other)
	if err != nil || v.State != "dispatched" {
		t.Fatal("reconnect changed rolled-back projection", v, err)
	}
	for _, want := range []int{1, 0} {
		err = database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
			count, err := wide.RecoverAmbiguousDispatched(ctx, tx, reconnect)
			if err == nil && count != want {
				t.Fatalf("reconnect count: %d, want %d", count, want)
			}
			return err
		})
		if err != nil {
			t.Fatal("common reconnect", err)
		}
	}
	v, err = wide.Get(ctx, other)
	if err != nil || v.State != "unknown" {
		t.Fatal("reconnect did not retain unknown", v, err)
	}
	if err := owner.QueryRow(ctx, `SELECT envelope FROM commands WHERE operation_id=?`, UUIDBytes(other)).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(stored, &envelope); err != nil || envelope.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY || envelope.ConnectionFence != nil || envelope.FenceBinding != nil || envelope.Authorization == nil {
		t.Fatal("reconnect envelope", &envelope, err)
	}
	reconnect.OwnerEpoch++
	err = database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error { _, err := wide.RecoverAmbiguousDispatched(ctx, tx, reconnect); return err })
	if !errors.Is(err, connectionowner.ErrNotOwner) {
		t.Fatal("stale reconnect authority", err)
	}
}
