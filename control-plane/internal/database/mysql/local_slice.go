package mysql

import (
	"context"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	localstore "github.com/GentleKingson/ocservia/control-plane/internal/localslice/store"
	"github.com/google/uuid"
)

type localSliceStore struct{ database.Tx }

func (t *transaction) LocalSliceStore() localstore.LocalStore { return localSliceStore{t} }

func (s localSliceStore) EnsureWorkspace(ctx context.Context, id uuid.UUID, slug string, at value.Timestamp) (uuid.UUID, error) {
	if err := LockExactKey(ctx, s.Tx, "workspaces"); err != nil {
		return uuid.Nil, err
	}
	var existing uuid.UUID
	err := s.QueryRow(ctx, `SELECT id FROM workspaces WHERE BINARY slug=BINARY ? FOR UPDATE`, slug).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, database.ErrNotFound) {
		return uuid.Nil, err
	}
	_, err = s.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'Local simulator',?,?,?)`, UUIDBytes(id), slug, at, at)
	return id, err
}

func (s localSliceStore) InsertSimulation(ctx context.Context, v localstore.Simulation) error {
	if _, err := s.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,?,'active',?,?)`, UUIDBytes(v.NodeID), UUIDBytes(v.WorkspaceID), "sim-"+v.NodeID.String(), v.At, v.At); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at)VALUES(?,?,'active',?)`, UUIDBytes(v.NodeID), v.Endpoint, v.At); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,command_id,state,request_id,trace_id,created_at,updated_at)VALUES(?,?,?,?,'queued',?,?,?,?)`, UUIDBytes(v.OperationID), UUIDBytes(v.WorkspaceID), UUIDBytes(v.NodeID), UUIDBytes(v.CommandID), v.RequestID, v.TraceID, v.At, v.At); err != nil {
		return err
	}
	_, err := s.Exec(ctx, `INSERT INTO local_slice_jobs(operation_id,command_envelope,traceparent,available_at,expires_at,created_at)VALUES(?,?,?,?,?,?)`, UUIDBytes(v.OperationID), v.Envelope, v.Traceparent, v.At, v.ExpiresAt, v.At)
	return err
}

func (s localSliceStore) Events(ctx context.Context, workspace, after uuid.UUID, limit int, descending bool) ([]localstore.Event, error) {
	var scope, cursor any
	if workspace != uuid.Nil {
		scope = UUIDBytes(workspace)
	}
	if after != uuid.Nil {
		cursor = UUIDBytes(after)
	}
	comparison, order := ">", "ASC"
	if descending {
		comparison, order = "<", "DESC"
	}
	rows, err := s.Query(ctx, `SELECT e.event_id,e.node_id,e.event_type,e.traceparent,e.occurred_at,e.ingest_sequence FROM transport_events e JOIN nodes n ON n.id=e.node_id WHERE (? IS NULL OR e.ingest_sequence `+comparison+` (SELECT ingest_sequence FROM transport_events WHERE event_id=?)) AND (? IS NULL OR n.workspace_id=?) ORDER BY e.ingest_sequence `+order+` LIMIT ?`, cursor, cursor, scope, scope, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]localstore.Event, 0, limit)
	for rows.Next() {
		var v localstore.Event
		var id, node uuid.UUID
		if err := rows.Scan(&id, &node, &v.Type, &v.Traceparent, &v.OccurredAt, &v.Sequence); err != nil {
			return nil, err
		}
		v.ID, v.NodeID = id.String(), node.String()
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s localSliceStore) EventSequence(ctx context.Context, workspace, event uuid.UUID) (v int64, err error) {
	err = s.QueryRow(ctx, `SELECT e.ingest_sequence FROM transport_events e JOIN nodes n ON n.id=e.node_id WHERE e.event_id=? AND n.workspace_id=?`, UUIDBytes(event), UUIDBytes(workspace)).Scan(&v)
	return
}

func (s localSliceStore) GapNodes(ctx context.Context, slug string) ([]localstore.GapNode, error) {
	rows, err := s.Query(ctx, `SELECT n.id,e.traceparent FROM nodes n JOIN workspaces w ON w.id=n.workspace_id JOIN transport_events e ON e.node_id=n.id AND e.ingest_sequence=(SELECT MAX(latest.ingest_sequence) FROM transport_events latest WHERE latest.node_id=n.id) WHERE BINARY w.slug=BINARY ? AND n.status='active'`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []localstore.GapNode
	for rows.Next() {
		var v localstore.GapNode
		if err := rows.Scan(&v.ID, &v.Traceparent); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s localSliceStore) InvalidateCursor(ctx context.Context) error {
	at, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE transport_events SET transport_cursor_valid=false WHERE transport_cursor_valid`); err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE transport_event_cursor SET valid=false,updated_at=? WHERE singleton`, at)
	return err
}

func (s localSliceStore) UnknownDispatched(ctx context.Context) error {
	at, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	n, err := s.Exec(ctx, `UPDATE operations o JOIN local_slice_jobs j ON j.operation_id=o.id SET o.state='unknown',o.updated_at=? WHERE j.dispatched_at IS NOT NULL AND o.state IN ('queued','dispatched','accepted','running')`, at)
	if err != nil || n == 0 {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE local_slice_jobs SET last_error='transport event retention gap; outcome requires reconciliation' WHERE dispatched_at IS NOT NULL AND operation_id IN (SELECT id FROM operations WHERE state='unknown')`)
	return err
}

func (s localSliceStore) DisconnectNode(ctx context.Context, node localstore.GapNode, event uuid.UUID, at value.Timestamp) error {
	n, err := s.Exec(ctx, `UPDATE nodes SET status='offline',updated_at=?,version=version+1 WHERE id=? AND status='active'`, at, UUIDBytes(node.ID))
	if err != nil || n == 0 {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload,transport_cursor_valid)VALUES(?,?,'disconnected',?,?,?,false)`, UUIDBytes(event), UUIDBytes(node.ID), at, node.Traceparent, []byte("transport event retention gap"))
	return err
}

func (s localSliceStore) ClaimJobs(ctx context.Context, limit int) ([]localstore.Job, error) {
	at, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return nil, err
	}
	next, err := at.Add(10 * time.Second)
	if err != nil {
		return nil, err
	}
	// Keep locking scoped to jobs. The node and operation reads are scalar
	// subqueries, matching PostgreSQL's FOR UPDATE OF job.
	rows, err := s.Query(ctx, `SELECT j.operation_id,(SELECT node_id FROM operations WHERE id=j.operation_id),j.command_envelope,j.traceparent FROM local_slice_jobs j WHERE j.available_at<=? AND j.expires_at>? AND j.dispatched_at IS NULL AND EXISTS(SELECT 1 FROM operations o WHERE o.id=j.operation_id AND o.state='queued') ORDER BY j.available_at,j.operation_id LIMIT ? FOR UPDATE SKIP LOCKED`, at, at, limit)
	if err != nil {
		return nil, err
	}
	values := make([]localstore.Job, 0, limit)
	for rows.Next() {
		var v localstore.Job
		if err := rows.Scan(&v.OperationID, &v.NodeID, &v.Envelope, &v.Traceparent); err != nil {
			rows.Close()
			return nil, err
		}
		values = append(values, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, v := range values {
		if _, err := s.Exec(ctx, `UPDATE local_slice_jobs SET available_at=? WHERE operation_id=?`, next, UUIDBytes(v.OperationID)); err != nil {
			return nil, err
		}
	}
	return values, nil
}

func (s localSliceStore) ExpireJobs(ctx context.Context) error {
	at, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	rows, err := s.Query(ctx, `SELECT o.id FROM operations o WHERE o.state NOT IN ('succeeded','failed','expired','rolled_back','superseded') AND EXISTS(SELECT 1 FROM local_slice_jobs j WHERE j.operation_id=o.id AND j.dispatched_at IS NULL AND j.expires_at<=?) FOR UPDATE`, at)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.Exec(ctx, `UPDATE operations SET state='expired',updated_at=?,version=version+1 WHERE id=?`, at, UUIDBytes(id)); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE local_slice_jobs SET last_error='command expired before dispatch' WHERE operation_id=?`, UUIDBytes(id)); err != nil {
			return err
		}
	}
	return nil
}

func (s localSliceStore) MarkDispatchStarted(ctx context.Context, id uuid.UUID) (bool, error) {
	at, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return false, err
	}
	n, err := s.Exec(ctx, `UPDATE operations SET state='dispatched',updated_at=?,version=version+1 WHERE id=? AND state='queued'`, at, UUIDBytes(id))
	if err != nil || n == 0 {
		return false, err
	}
	n, err = s.Exec(ctx, `UPDATE local_slice_jobs SET dispatched_at=?,attempts=attempts+1,last_error=NULL WHERE operation_id=? AND dispatched_at IS NULL`, at, UUIDBytes(id))
	return n == 1, err
}

func (s localSliceStore) MarkDispatchError(ctx context.Context, id uuid.UUID, message string) error {
	at, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	next, err := at.Add(time.Second)
	if err != nil {
		return err
	}
	n, err := s.Exec(ctx, `UPDATE operations SET state='queued',updated_at=?,version=version+1 WHERE id=? AND state='dispatched'`, at, UUIDBytes(id))
	if err != nil || n == 0 {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE local_slice_jobs SET dispatched_at=NULL,last_error=?,available_at=? WHERE operation_id=?`, message, next, UUIDBytes(id))
	return err
}
