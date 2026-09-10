package mysql

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func TestRealTrustConvergence(t *testing.T) {
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
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	within := func(action func(enrollmentstore.TrustStore) error) error {
		return database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
			store, err := enrollmentstore.Trust(tx)
			if err != nil {
				return err
			}
			return action(store)
		})
	}
	negative := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}
	positive := value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	workspace, workerID := uuid.New(), uuid.New()
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'trust',?,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, UUIDBytes(workspace), workspace.String())
	newJob := func() enrollmentstore.TrustJob {
		t.Helper()
		node := uuid.Must(uuid.NewV7())
		run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,?,'active',TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, UUIDBytes(node), UUIDBytes(workspace), node.String())
		return enrollmentstore.TrustJob{NodeID: node, EndpointID: bytes.Repeat([]byte{1}, 32), DesiredState: "active", Revision: 1, Reason: "approve"}
	}
	enqueue := func(v enrollmentstore.TrustJob, at value.Timestamp) {
		t.Helper()
		if err := within(func(s enrollmentstore.TrustStore) error { return s.Enqueue(ctx, v, at) }); err != nil {
			t.Fatal(err)
		}
	}
	type snapshot struct {
		State, Reason                      string
		Revision                           uint64
		Update, CloseRequired, Close       bool
		Available, Until, Created, Updated value.Timestamp
		Worker                             *uuid.UUID
		Attempts                           int
		LastError                          *string
	}
	read := func(node uuid.UUID) snapshot {
		t.Helper()
		var v snapshot
		if err := backend.QueryRow(ctx, `SELECT desired_state,reason,revision,update_applied,close_required,close_applied,available_at,locked_until,created_at,updated_at,locked_by,attempts,last_error FROM node_trust_convergence WHERE node_id=?`, UUIDBytes(node)).Scan(&v.State, &v.Reason, &v.Revision, &v.Update, &v.CloseRequired, &v.Close, &v.Available, &v.Until, &v.Created, &v.Updated, &v.Worker, &v.Attempts, &v.LastError); err != nil {
			t.Fatal(err)
		}
		return v
	}
	first, second := newJob(), newJob()
	enqueue(first, negative)
	enqueue(second, negative)
	locked, err := backend.Begin(ctx, database.ReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback(context.Background())
	var id uuid.UUID
	if err := locked.QueryRow(ctx, `SELECT node_id FROM node_trust_convergence WHERE node_id=? FOR UPDATE`, UUIDBytes(first.NodeID)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	err = database.Within(bounded, backend, database.ReadCommitted, func(tx database.Tx) error {
		s, err := enrollmentstore.Trust(tx)
		if err != nil {
			return err
		}
		second, err = s.Claim(bounded, workerID)
		return err
	})
	cancel()
	if err != nil || second.NodeID == first.NodeID || second.Attempts != 1 {
		t.Fatal("locked job was not skipped", second, err)
	}
	if err := locked.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	abort := errors.New("abort claim")
	err = within(func(s enrollmentstore.TrustStore) error {
		job, err := s.Claim(ctx, workerID)
		if err != nil || job.NodeID != first.NodeID {
			t.Fatal("claim before rollback", job, err)
		}
		return abort
	})
	if !errors.Is(err, abort) || read(first.NodeID).Attempts != 0 || read(first.NodeID).Worker != nil {
		t.Fatal("claim partially committed", err)
	}
	if err := within(func(s enrollmentstore.TrustStore) error {
		var err error
		first, err = s.Claim(ctx, workerID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	claim := read(first.NodeID)
	if claim.Until.Micros-claim.Updated.Micros != 10_000_000 || claim.Attempts != 1 || claim.Worker == nil || *claim.Worker != workerID {
		t.Fatal("claim clock or ownership", claim)
	}
	if err := within(func(s enrollmentstore.TrustStore) error { _, err := s.Claim(ctx, workerID); return err }); !errors.Is(err, database.ErrNotFound) {
		t.Fatal("leased jobs claimed twice", err)
	}
	if err := within(func(s enrollmentstore.TrustStore) error {
		if changed, err := s.MarkUpdateApplied(ctx, first, uuid.New()); err != nil || changed {
			t.Fatal("wrong worker marked update", changed, err)
		}
		for range 2 {
			if changed, err := s.MarkUpdateApplied(ctx, first, workerID); err != nil || !changed {
				t.Fatal("repeated mark lost matched-row semantics", changed, err)
			}
		}
		return s.UnlockComplete(ctx, first, workerID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := within(func(s enrollmentstore.TrustStore) error {
		return s.Release(ctx, second, workerID, 2*time.Second, "retry")
	}); err != nil {
		t.Fatal(err)
	}
	released := read(second.NodeID)
	if released.Worker != nil || released.Until.Valid || released.Available.Micros-released.Updated.Micros != 2_000_000 || released.LastError == nil || *released.LastError != "retry" {
		t.Fatal("retry release", released)
	}
	run(`UPDATE node_trust_convergence SET available_at=? WHERE node_id=?`, negative, UUIDBytes(second.NodeID))
	if err := within(func(s enrollmentstore.TrustStore) error {
		v, err := s.Claim(ctx, workerID)
		if err != nil {
			return err
		}
		if changed, err := s.MarkUpdateApplied(ctx, v, workerID); err != nil || !changed {
			t.Fatal(changed, err)
		}
		return s.UnlockComplete(ctx, v, workerID)
	}); err != nil {
		t.Fatal(err)
	}

	// Higher intent invalidates an in-flight worker, while equal/older intent
	// cannot reset its flags, clocks, reason, or revision.
	first.DesiredState, first.Revision, first.Reason = "revoked", 2, "revoke"
	enqueue(first, negative)
	if err := within(func(s enrollmentstore.TrustStore) error {
		var err error
		first, err = s.Claim(ctx, workerID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	newer := first
	newer.DesiredState, newer.Revision, newer.Reason = "active", 3, "newer"
	enqueue(newer, negative)
	before := read(first.NodeID)
	first.Reason = "must not replace"
	enqueue(first, positive)
	equal := newer
	equal.Reason = "must not replace equal"
	enqueue(equal, positive)
	if err := within(func(s enrollmentstore.TrustStore) error {
		if changed, err := s.MarkUpdateApplied(ctx, first, workerID); err != nil || changed {
			t.Fatal("stale update", changed, err)
		}
		if changed, err := s.MarkCloseApplied(ctx, first, workerID); err != nil || changed {
			t.Fatal("stale close", changed, err)
		}
		if err := s.Release(ctx, first, workerID, time.Minute, "stale"); err != nil {
			return err
		}
		return s.UnlockComplete(ctx, first, workerID)
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, read(first.NodeID)) {
		t.Fatal("superseded worker or old intent changed newer revision")
	}
	err = within(func(s enrollmentstore.TrustStore) error {
		aborted := newer
		aborted.Revision = 99
		if err := s.Enqueue(ctx, aborted, positive); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) || !reflect.DeepEqual(before, read(first.NodeID)) {
		t.Fatal("enqueue escaped caller transaction", err)
	}
	transport := &trustProbe{t: t, node: first.NodeID}
	fences := &trustFenceProbe{t: t, node: first.NodeID, transport: transport}
	worker, err := enrollment.NewFencedTrustConvergenceWorkerBackend(backend, transport, fences, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.RunOnce(ctx); err != nil || !worked {
		t.Fatal("active convergence", worked, err)
	}
	if v := read(first.NodeID); !v.Update || v.Close || v.Worker != nil || transport.closes != 0 {
		t.Fatal("active convergence closed node", v)
	}
	newer.DesiredState, newer.Revision, newer.Reason = "revoked", 4, "revoke again"
	enqueue(newer, negative)
	transport.updates, transport.closes = 0, 0
	transport.updateFailures, transport.closeFailures = 1, 1
	transport.operations = nil
	for attempt := range 3 {
		worked, err := worker.RunOnce(ctx)
		if !worked || (err != nil) != (attempt < 2) {
			t.Fatal("retry convergence", attempt, worked, err)
		}
		v := read(first.NodeID)
		if v.Update != (attempt > 0) || v.Close != (attempt == 2) || v.Worker != nil || v.Until.Valid {
			t.Fatal("partial progress or released lock", attempt, v)
		}
		if attempt < 2 {
			if v.LastError == nil || v.Available.Micros-v.Updated.Micros != int64(1<<min(v.Attempts, 6))*1_000_000 {
				t.Fatal("worker backoff", v)
			}
			run(`UPDATE node_trust_convergence SET available_at=? WHERE node_id=?`, negative, UUIDBytes(first.NodeID))
		}
	}
	if transport.updates != 2 || transport.closes != 2 || len(transport.operations) != 2 || transport.operations[0] != transport.operations[1] {
		t.Fatal("retry repeated completed work or changed stable operation ID", transport)
	}
	if worked, err := worker.RunOnce(ctx); err != nil || worked {
		t.Fatal("completed work claimed", worked, err)
	}
	newer.Revision++
	enqueue(newer, negative)
	fences.deny = true
	if worked, err := worker.RunOnce(ctx); err == nil || !worked || transport.updates != 2 || read(first.NodeID).Update {
		t.Fatal("fencing failure reached transport", worked, err)
	}
	fences.deny = false
	// Infinite future availability and lease are never runnable. Full finite
	// PostgreSQL boundaries remain stored literally, not native DATETIME.
	for _, at := range []value.Timestamp{positive, {Valid: true, Micros: value.EndTimestamp - 1}} {
		newer.Revision++
		enqueue(newer, at)
		if read(first.NodeID).Available != at || read(first.NodeID).Updated != at {
			t.Fatal("wide timestamp lost")
		}
		if worked, err := worker.RunOnce(ctx); err != nil || worked {
			t.Fatal("future work claimed", at, worked, err)
		}
	}
	run(`UPDATE node_trust_convergence SET available_at=?,locked_by=?,locked_until=? WHERE node_id=?`, negative, UUIDBytes(workerID), positive, UUIDBytes(first.NodeID))
	if worked, err := worker.RunOnce(ctx); err != nil || worked {
		t.Fatal("infinite lease claimed", worked, err)
	}
	run(`UPDATE node_trust_convergence SET available_at=?,locked_until=? WHERE node_id=?`, value.Timestamp{Valid: true, Micros: value.MinTimestamp}, negative, UUIDBytes(first.NodeID))
	if worked, err := worker.RunOnce(ctx); err != nil || !worked {
		t.Fatal("expired lease or earliest finite timestamp", worked, err)
	}
	if _, err := backend.Exec(ctx, `UPDATE node_trust_convergence SET locked_until=? WHERE node_id=?`, positive, UUIDBytes(first.NodeID)); !errors.Is(err, database.ErrConstraint) {
		t.Fatal("lock-pair constraint lost", err)
	}
	if _, err := backend.Exec(ctx, `UPDATE node_trust_convergence SET available_at=? WHERE node_id=?`, value.EndTimestamp, UUIDBytes(first.NodeID)); !errors.Is(err, database.ErrConstraint) {
		t.Fatal("timestamp domain constraint lost", err)
	}
	concurrent := newJob()
	start := make(chan struct{})
	results := make(chan error, 3)
	for _, revision := range []uint64{3, 1, 2} {
		go func() {
			<-start
			job := concurrent
			job.Revision = revision
			results <- within(func(s enrollmentstore.TrustStore) error { return s.Enqueue(ctx, job, positive) })
		}()
	}
	close(start)
	var enqueueErrors error
	for range 3 {
		enqueueErrors = errors.Join(enqueueErrors, <-results)
	}
	if enqueueErrors != nil {
		t.Fatal("concurrent first enqueue", enqueueErrors)
	}
	if v := read(concurrent.NodeID); v.Revision != 3 || v.Attempts != 0 || v.Available != positive || v.Created != positive {
		t.Fatal("concurrent first enqueue lost highest revision", v)
	}
}

type trustProbe struct {
	t                             *testing.T
	node                          uuid.UUID
	binding                       *agentv1.FenceBindingV2
	updates, closes               int
	updateFailures, closeFailures int
	operations                    [][16]byte
}

func (p *trustProbe) UpdateNodeTrust(_ context.Context, node, endpoint []byte, state transportv1.NodeTrustState, reason string, revision uint64, operation []byte, binding *agentv1.FenceBindingV2) error {
	p.t.Helper()
	want := ownersession.StateUpdateOperationID([16]byte(p.node), endpoint, int32(state), revision, reason)
	if binding == nil || binding != p.binding || !bytes.Equal(node, p.node[:]) || !bytes.Equal(operation, want[:]) {
		p.t.Fatal("trust mutation escaped fencing or changed operation ID")
	}
	p.updates++
	p.operations = append(p.operations, want)
	if p.updateFailures > 0 {
		p.updateFailures--
		return errors.New("transport update unavailable")
	}
	return nil
}

func (p *trustProbe) CloseNode(_ context.Context, node []byte, reason string, binding *agentv1.FenceBindingV2) error {
	p.t.Helper()
	if binding == nil || binding != p.binding || !bytes.Equal(node, p.node[:]) || reason != "node revoked" {
		p.t.Fatal("close mutation escaped fencing")
	}
	p.closes++
	if p.closeFailures > 0 {
		p.closeFailures--
		return errors.New("transport close unavailable")
	}
	return nil
}

type trustFenceProbe struct {
	t         *testing.T
	node      uuid.UUID
	transport *trustProbe
	deny      bool
}

func (f *trustFenceProbe) ExecuteFenced(ctx context.Context, node [16]byte, kind agentv1.FenceOperationKind, operation [16]byte, capability string, action ownersession.FencedAction) error {
	f.t.Helper()
	if node != [16]byte(f.node) || capability != ownersession.FencingCapability || (kind != agentv1.FenceOperationKind_FENCE_OPERATION_KIND_STATE_UPDATE && kind != agentv1.FenceOperationKind_FENCE_OPERATION_KIND_CONNECTION_CLOSE) || (kind == agentv1.FenceOperationKind_FENCE_OPERATION_KIND_CONNECTION_CLOSE && operation != node) {
		f.t.Fatal("incorrect administrative fence request")
	}
	if f.deny {
		return errors.New("owner term lost")
	}
	f.transport.binding = &agentv1.FenceBindingV2{}
	defer func() { f.transport.binding = nil }()
	return action(ctx, nil, f.transport.binding)
}
