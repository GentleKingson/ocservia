package postgres

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	localstore "github.com/GentleKingson/ocservia/control-plane/internal/localslice/store"
	"github.com/google/uuid"
)

type localSliceStore struct{ database.Tx }

func (t *transaction) LocalSliceStore() localstore.LocalStore { return localSliceStore{t} }

func (s localSliceStore) EnsureWorkspace(ctx context.Context, id uuid.UUID, slug string, at value.Timestamp) (uuid.UUID, error) {
	err := s.QueryRow(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'Local simulator',$2,$3,$3) ON CONFLICT(slug) DO UPDATE SET slug=EXCLUDED.slug RETURNING id`, id, slug, at).Scan(&id)
	return id, err
}

func (s localSliceStore) InsertSimulation(ctx context.Context, v localstore.Simulation) error {
	if _, err := s.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES($1,$2,$3,'active',$4,$4)`, v.NodeID, v.WorkspaceID, "sim-"+v.NodeID.String(), v.At); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at)VALUES($1,$2,'active',$3)`, v.NodeID, v.Endpoint, v.At); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,command_id,state,request_id,trace_id,created_at,updated_at)VALUES($1,$2,$3,$4,'queued',$5,$6,$7,$7)`, v.OperationID, v.WorkspaceID, v.NodeID, v.CommandID, v.RequestID, v.TraceID, v.At); err != nil {
		return err
	}
	_, err := s.Exec(ctx, `INSERT INTO local_slice_jobs(operation_id,command_envelope,traceparent,available_at,expires_at,created_at)VALUES($1,$2,$3,$4,$5,$4)`, v.OperationID, v.Envelope, v.Traceparent, v.At, v.ExpiresAt)
	return err
}

func (s localSliceStore) Events(ctx context.Context, workspace, after uuid.UUID, limit int, descending bool) ([]localstore.Event, error) {
	var scope, cursor any
	if workspace != uuid.Nil {
		scope = workspace
	}
	if after != uuid.Nil {
		cursor = after
	}
	comparison, order := ">", "ASC"
	if descending {
		comparison, order = "<", "DESC"
	}
	rows, err := s.Query(ctx, `SELECT e.event_id::text,e.node_id::text,e.event_type,e.traceparent,e.occurred_at,e.ingest_sequence FROM transport_events e JOIN nodes n ON n.id=e.node_id WHERE ($1::uuid IS NULL OR e.ingest_sequence `+comparison+` (SELECT ingest_sequence FROM transport_events WHERE event_id=$1)) AND ($2::uuid IS NULL OR n.workspace_id=$2) ORDER BY e.ingest_sequence `+order+` LIMIT $3`, cursor, scope, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]localstore.Event, 0, limit)
	for rows.Next() {
		var v localstore.Event
		if err := rows.Scan(&v.ID, &v.NodeID, &v.Type, &v.Traceparent, &v.OccurredAt, &v.Sequence); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s localSliceStore) EventSequence(ctx context.Context, workspace, event uuid.UUID) (v int64, err error) {
	err = s.QueryRow(ctx, `SELECT e.ingest_sequence FROM transport_events e JOIN nodes n ON n.id=e.node_id WHERE e.event_id=$1 AND n.workspace_id=$2`, event, workspace).Scan(&v)
	return
}

func (s localSliceStore) GapNodes(ctx context.Context, slug string) ([]localstore.GapNode, error) {
	rows, err := s.Query(ctx, `SELECT n.id,e.traceparent FROM nodes n JOIN workspaces w ON w.id=n.workspace_id JOIN LATERAL (SELECT traceparent FROM transport_events WHERE node_id=n.id ORDER BY ingest_sequence DESC LIMIT 1) e ON true WHERE w.slug=$1 AND n.status='active'`, slug)
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
	if _, err := s.Exec(ctx, `UPDATE transport_events SET transport_cursor_valid=false WHERE transport_cursor_valid`); err != nil {
		return err
	}
	_, err := s.Exec(ctx, `UPDATE transport_event_cursor SET valid=false,updated_at=now() WHERE singleton`)
	return err
}

func (s localSliceStore) UnknownDispatched(ctx context.Context) error {
	n, err := s.Exec(ctx, `UPDATE operations o SET state='unknown',updated_at=now() FROM local_slice_jobs j WHERE j.operation_id=o.id AND j.dispatched_at IS NOT NULL AND o.state IN ('queued','dispatched','accepted','running')`)
	if err != nil || n == 0 {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE local_slice_jobs SET last_error='transport event retention gap; outcome requires reconciliation' WHERE dispatched_at IS NOT NULL AND operation_id IN (SELECT id FROM operations WHERE state='unknown')`)
	return err
}

func (s localSliceStore) DisconnectNode(ctx context.Context, node localstore.GapNode, event uuid.UUID, at value.Timestamp) error {
	n, err := s.Exec(ctx, `UPDATE nodes SET status='offline',updated_at=$2,version=version+1 WHERE id=$1 AND status='active'`, node.ID, at)
	if err != nil || n == 0 {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload,transport_cursor_valid)VALUES($1,$2,'disconnected',$3,$4,$5,false)`, event, node.ID, at, node.Traceparent, []byte("transport event retention gap"))
	return err
}

func (s localSliceStore) ClaimJobs(ctx context.Context, limit int) ([]localstore.Job, error) {
	rows, err := s.Query(ctx, `WITH candidates AS (SELECT j.operation_id FROM local_slice_jobs j JOIN operations o ON o.id=j.operation_id WHERE j.available_at<=now() AND j.expires_at>now() AND j.dispatched_at IS NULL AND o.state='queued' ORDER BY j.available_at,j.operation_id FOR UPDATE OF j SKIP LOCKED LIMIT $1) UPDATE local_slice_jobs j SET available_at=now()+interval '10 seconds' FROM candidates c WHERE j.operation_id=c.operation_id RETURNING j.operation_id,(SELECT node_id FROM operations WHERE id=j.operation_id),j.command_envelope,j.traceparent`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]localstore.Job, 0, limit)
	for rows.Next() {
		var v localstore.Job
		if err := rows.Scan(&v.OperationID, &v.NodeID, &v.Envelope, &v.Traceparent); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s localSliceStore) ExpireJobs(ctx context.Context) error {
	_, err := s.Exec(ctx, `WITH expired AS (UPDATE operations o SET state='expired',updated_at=now(),version=version+1 FROM local_slice_jobs j WHERE o.id=j.operation_id AND j.dispatched_at IS NULL AND j.expires_at<=now() AND o.state NOT IN ('succeeded','failed','expired','rolled_back','superseded') RETURNING o.id) UPDATE local_slice_jobs j SET last_error='command expired before dispatch' FROM expired e WHERE j.operation_id=e.id`)
	return err
}

func (s localSliceStore) MarkDispatchStarted(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := s.Exec(ctx, `WITH started AS (UPDATE operations SET state='dispatched',updated_at=now(),version=version+1 WHERE id=$1 AND state='queued' RETURNING id) UPDATE local_slice_jobs j SET dispatched_at=now(),attempts=attempts+1,last_error=NULL FROM started s WHERE j.operation_id=s.id AND j.dispatched_at IS NULL`, id)
	return n == 1, err
}

func (s localSliceStore) MarkDispatchError(ctx context.Context, id uuid.UUID, message string) error {
	_, err := s.Exec(ctx, `WITH retryable AS (UPDATE operations SET state='queued',updated_at=now(),version=version+1 WHERE id=$1 AND state='dispatched' RETURNING id) UPDATE local_slice_jobs j SET dispatched_at=NULL,last_error=$2,available_at=now()+interval '1 second' FROM retryable r WHERE j.operation_id=r.id`, id, message)
	return err
}
