package postgres

import (
	"context"
	"time"

	devsimstore "github.com/GentleKingson/ocservia/control-plane/internal/devsim/store"
	"github.com/google/uuid"
)

type devSimStore struct{ *transaction }

func (t *transaction) DevSimStore() devsimstore.Store { return devSimStore{t} }

func (s devSimStore) EnsureWorkspace(ctx context.Context, id uuid.UUID, slug string, at time.Time) (uuid.UUID, error) {
	err := s.QueryRow(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'Local simulator',$2,$3,$3) ON CONFLICT(slug) DO UPDATE SET slug=EXCLUDED.slug RETURNING id`, id, slug, at).Scan(&id)
	return id, err
}

func (s devSimStore) InsertSimulation(ctx context.Context, v devsimstore.Simulation) error {
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

func (s devSimStore) GapNodes(ctx context.Context, slug string) ([]devsimstore.GapNode, error) {
	return collectRows(ctx, s.store, `SELECT n.id,e.traceparent FROM nodes n JOIN workspaces w ON w.id=n.workspace_id JOIN LATERAL (SELECT traceparent FROM transport_events WHERE node_id=n.id ORDER BY ingest_sequence DESC LIMIT 1) e ON true WHERE w.slug=$1 AND n.status='active'`, func(r Rows) (v devsimstore.GapNode, err error) {
		err = r.Scan(&v.ID, &v.Traceparent)
		return
	}, slug)
}

func (s devSimStore) UnknownDispatched(ctx context.Context) error {
	n, err := s.Exec(ctx, `UPDATE operations o SET state='unknown',updated_at=now() FROM local_slice_jobs j WHERE j.operation_id=o.id AND j.dispatched_at IS NOT NULL AND o.state IN ('queued','dispatched','accepted','running')`)
	if err != nil || n == 0 {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE local_slice_jobs SET last_error='transport event retention gap; outcome requires reconciliation' WHERE dispatched_at IS NOT NULL AND operation_id IN (SELECT id FROM operations WHERE state='unknown')`)
	return err
}

func (s devSimStore) DisconnectNode(ctx context.Context, node devsimstore.GapNode, event uuid.UUID, at time.Time) error {
	n, err := s.Exec(ctx, `UPDATE nodes SET status='offline',updated_at=$2,version=version+1 WHERE id=$1 AND status='active'`, node.ID, at)
	if err != nil || n == 0 {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload,transport_cursor_valid)VALUES($1,$2,'disconnected',$3,$4,$5,false)`, event, node.ID, at, node.Traceparent, []byte("transport event retention gap"))
	return err
}

func (s devSimStore) ClaimSimulationJobs(ctx context.Context, limit int) ([]devsimstore.SimulationJob, error) {
	return collectRows(ctx, s.store, `WITH candidates AS (SELECT j.operation_id FROM local_slice_jobs j JOIN operations o ON o.id=j.operation_id WHERE j.available_at<=now() AND j.expires_at>now() AND j.dispatched_at IS NULL AND o.state='queued' ORDER BY j.available_at,j.operation_id FOR UPDATE OF j SKIP LOCKED LIMIT $1) UPDATE local_slice_jobs j SET available_at=now()+interval '10 seconds' FROM candidates c WHERE j.operation_id=c.operation_id RETURNING j.operation_id,(SELECT node_id FROM operations WHERE id=j.operation_id),j.command_envelope,j.traceparent`, func(r Rows) (v devsimstore.SimulationJob, err error) {
		err = r.Scan(&v.OperationID, &v.NodeID, &v.Envelope, &v.Traceparent)
		return
	}, limit)
}

func (s devSimStore) ExpireSimulationJobs(ctx context.Context) error {
	_, err := s.Exec(ctx, `WITH expired AS (UPDATE operations o SET state='expired',updated_at=now(),version=version+1 FROM local_slice_jobs j WHERE o.id=j.operation_id AND j.dispatched_at IS NULL AND j.expires_at<=now() AND o.state NOT IN ('succeeded','failed','expired','rolled_back','superseded') RETURNING o.id) UPDATE local_slice_jobs j SET last_error='command expired before dispatch' FROM expired e WHERE j.operation_id=e.id`)
	return err
}

func (s devSimStore) MarkSimulationDispatchStarted(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := s.Exec(ctx, `WITH started AS (UPDATE operations SET state='dispatched',updated_at=now(),version=version+1 WHERE id=$1 AND state='queued' RETURNING id) UPDATE local_slice_jobs j SET dispatched_at=now(),attempts=attempts+1,last_error=NULL FROM started s WHERE j.operation_id=s.id AND j.dispatched_at IS NULL`, id)
	return n == 1, err
}

func (s devSimStore) MarkSimulationDispatchError(ctx context.Context, id uuid.UUID, message string) error {
	_, err := s.Exec(ctx, `WITH retryable AS (UPDATE operations SET state='queued',updated_at=now(),version=version+1 WHERE id=$1 AND state='dispatched' RETURNING id) UPDATE local_slice_jobs j SET dispatched_at=NULL,last_error=$2,available_at=now()+interval '1 second' FROM retryable r WHERE j.operation_id=r.id`, id, message)
	return err
}

func (s devSimStore) SimulationOutcome(ctx context.Context, node uuid.UUID, state, traceparent string, disconnected bool, at time.Time) error {
	if disconnected {
		_, err := s.Exec(ctx, `UPDATE operations o SET state='unknown',updated_at=$2,version=version+1 FROM local_slice_jobs j WHERE o.id=j.operation_id AND o.node_id=$1 AND j.dispatched_at IS NOT NULL AND o.state NOT IN ('succeeded','failed','expired','rolled_back','superseded')`, node, at)
		return err
	}
	_, err := s.Exec(ctx, `UPDATE operations o SET state=$2,updated_at=$3,version=version+1 FROM local_slice_jobs j WHERE o.id=j.operation_id AND o.node_id=$1 AND j.traceparent=$4 AND o.state NOT IN ('succeeded','failed','expired','rolled_back','superseded')`, node, state, at, traceparent)
	return err
}
