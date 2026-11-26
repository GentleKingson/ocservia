package postgres

import (
	"context"
	"time"

	transportstore "github.com/GentleKingson/ocservia/control-plane/internal/transportevents/store"
	"github.com/google/uuid"
)

type transportEventStore struct{ *transaction }

func (t *transaction) TransportEventStore() transportstore.Store { return transportEventStore{t} }

func (s transportEventStore) EnsureWorkspace(ctx context.Context, id uuid.UUID, slug string, at time.Time) (uuid.UUID, error) {
	err := s.QueryRow(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'Local simulator',$2,$3,$3) ON CONFLICT(slug) DO UPDATE SET slug=EXCLUDED.slug RETURNING id`, id, slug, at).Scan(&id)
	return id, err
}

func (s transportEventStore) InsertSimulation(ctx context.Context, v transportstore.Simulation) error {
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

func (s transportEventStore) Events(ctx context.Context, workspace, after uuid.UUID, limit int, descending bool) ([]transportstore.Event, error) {
	comparison, order := ">", "ASC"
	if descending {
		comparison, order = "<", "DESC"
	}
	const base = `SELECT e.event_id::text,e.node_id::text,e.event_type,e.traceparent,e.occurred_at,e.ingest_sequence FROM transport_events e JOIN nodes n ON n.id=e.node_id`
	var suffix string
	var args []any
	switch {
	case workspace == uuid.Nil && after == uuid.Nil:
		suffix, args = ` ORDER BY e.ingest_sequence `+order+` LIMIT $1`, []any{limit}
	case workspace == uuid.Nil:
		suffix, args = ` WHERE e.ingest_sequence `+comparison+` (SELECT ingest_sequence FROM transport_events WHERE event_id=$1) ORDER BY e.ingest_sequence `+order+` LIMIT $2`, []any{after, limit}
	case after == uuid.Nil:
		suffix, args = ` WHERE n.workspace_id=$1 ORDER BY e.ingest_sequence `+order+` LIMIT $2`, []any{workspace, limit}
	default:
		suffix, args = ` WHERE e.ingest_sequence `+comparison+` (SELECT c.ingest_sequence FROM transport_events c JOIN nodes cn ON cn.id=c.node_id WHERE c.event_id=$1 AND cn.workspace_id=$2) AND n.workspace_id=$2 ORDER BY e.ingest_sequence `+order+` LIMIT $3`, []any{after, workspace, limit}
	}
	return collectRows(ctx, s.store, base+suffix, func(r Rows) (v transportstore.Event, err error) {
		err = r.Scan(&v.ID, &v.NodeID, &v.Type, &v.Traceparent, &v.OccurredAt, &v.Sequence)
		return
	}, args...)
}

func (s transportEventStore) EventSequence(ctx context.Context, workspace, event uuid.UUID) (v int64, err error) {
	err = s.QueryRow(ctx, `SELECT e.ingest_sequence FROM transport_events e JOIN nodes n ON n.id=e.node_id WHERE e.event_id=$1 AND n.workspace_id=$2`, event, workspace).Scan(&v)
	return
}

func (s transportEventStore) GapNodes(ctx context.Context, slug string) ([]transportstore.GapNode, error) {
	return collectRows(ctx, s.store, `SELECT n.id,e.traceparent FROM nodes n JOIN workspaces w ON w.id=n.workspace_id JOIN LATERAL (SELECT traceparent FROM transport_events WHERE node_id=n.id ORDER BY ingest_sequence DESC LIMIT 1) e ON true WHERE w.slug=$1 AND n.status='active'`, func(r Rows) (v transportstore.GapNode, err error) {
		err = r.Scan(&v.ID, &v.Traceparent)
		return
	}, slug)
}

func (s transportEventStore) InvalidateCursor(ctx context.Context) error {
	if _, err := s.Exec(ctx, `UPDATE transport_events SET transport_cursor_valid=false WHERE transport_cursor_valid`); err != nil {
		return err
	}
	_, err := s.Exec(ctx, `UPDATE transport_event_cursor SET valid=false,updated_at=now() WHERE singleton`)
	return err
}

func (s transportEventStore) UnknownDispatched(ctx context.Context) error {
	n, err := s.Exec(ctx, `UPDATE operations o SET state='unknown',updated_at=now() FROM local_slice_jobs j WHERE j.operation_id=o.id AND j.dispatched_at IS NOT NULL AND o.state IN ('queued','dispatched','accepted','running')`)
	if err != nil || n == 0 {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE local_slice_jobs SET last_error='transport event retention gap; outcome requires reconciliation' WHERE dispatched_at IS NOT NULL AND operation_id IN (SELECT id FROM operations WHERE state='unknown')`)
	return err
}

func (s transportEventStore) DisconnectNode(ctx context.Context, node transportstore.GapNode, event uuid.UUID, at time.Time) error {
	n, err := s.Exec(ctx, `UPDATE nodes SET status='offline',updated_at=$2,version=version+1 WHERE id=$1 AND status='active'`, node.ID, at)
	if err != nil || n == 0 {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload,transport_cursor_valid)VALUES($1,$2,'disconnected',$3,$4,$5,false)`, event, node.ID, at, node.Traceparent, []byte("transport event retention gap"))
	return err
}

func (s transportEventStore) ClaimSimulationJobs(ctx context.Context, limit int) ([]transportstore.SimulationJob, error) {
	return collectRows(ctx, s.store, `WITH candidates AS (SELECT j.operation_id FROM local_slice_jobs j JOIN operations o ON o.id=j.operation_id WHERE j.available_at<=now() AND j.expires_at>now() AND j.dispatched_at IS NULL AND o.state='queued' ORDER BY j.available_at,j.operation_id FOR UPDATE OF j SKIP LOCKED LIMIT $1) UPDATE local_slice_jobs j SET available_at=now()+interval '10 seconds' FROM candidates c WHERE j.operation_id=c.operation_id RETURNING j.operation_id,(SELECT node_id FROM operations WHERE id=j.operation_id),j.command_envelope,j.traceparent`, func(r Rows) (v transportstore.SimulationJob, err error) {
		err = r.Scan(&v.OperationID, &v.NodeID, &v.Envelope, &v.Traceparent)
		return
	}, limit)
}

func (s transportEventStore) ExpireSimulationJobs(ctx context.Context) error {
	_, err := s.Exec(ctx, `WITH expired AS (UPDATE operations o SET state='expired',updated_at=now(),version=version+1 FROM local_slice_jobs j WHERE o.id=j.operation_id AND j.dispatched_at IS NULL AND j.expires_at<=now() AND o.state NOT IN ('succeeded','failed','expired','rolled_back','superseded') RETURNING o.id) UPDATE local_slice_jobs j SET last_error='command expired before dispatch' FROM expired e WHERE j.operation_id=e.id`)
	return err
}

func (s transportEventStore) MarkSimulationDispatchStarted(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := s.Exec(ctx, `WITH started AS (UPDATE operations SET state='dispatched',updated_at=now(),version=version+1 WHERE id=$1 AND state='queued' RETURNING id) UPDATE local_slice_jobs j SET dispatched_at=now(),attempts=attempts+1,last_error=NULL FROM started s WHERE j.operation_id=s.id AND j.dispatched_at IS NULL`, id)
	return n == 1, err
}

func (s transportEventStore) MarkSimulationDispatchError(ctx context.Context, id uuid.UUID, message string) error {
	_, err := s.Exec(ctx, `WITH retryable AS (UPDATE operations SET state='queued',updated_at=now(),version=version+1 WHERE id=$1 AND state='dispatched' RETURNING id) UPDATE local_slice_jobs j SET dispatched_at=NULL,last_error=$2,available_at=now()+interval '1 second' FROM retryable r WHERE j.operation_id=r.id`, id, message)
	return err
}

type transportIngressStore struct{ *transaction }

func (t *transaction) TransportIngressStore() transportstore.IngressStore {
	return transportIngressStore{t}
}
func (s transportIngressStore) SaveBusiness(ctx context.Context) error {
	_, err := s.Exec(ctx, `SAVEPOINT transport_event_business`)
	return err
}
func (s transportIngressStore) RollbackBusiness(ctx context.Context) error {
	_, err := s.Exec(ctx, `ROLLBACK TO SAVEPOINT transport_event_business`)
	return err
}
func (s transportIngressStore) ReleaseBusiness(ctx context.Context) error {
	_, err := s.Exec(ctx, `RELEASE SAVEPOINT transport_event_business`)
	return err
}
func (s transportIngressStore) LockTrust(ctx context.Context, node uuid.UUID, endpoint []byte) (v transportstore.IngressTrust, err error) {
	err = s.QueryRow(ctx, `SELECT n.workspace_id,n.status,k.state FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.id=$1 AND k.endpoint_id=$2 FOR NO KEY UPDATE OF n,k`, node, endpoint).Scan(&v.WorkspaceID, &v.NodeStatus, &v.EndpointState)
	return
}
func (s transportIngressStore) LockTrustShared(ctx context.Context, node uuid.UUID, endpoint []byte) (v transportstore.IngressTrust, err error) {
	err = s.QueryRow(ctx, `SELECT n.workspace_id,n.status,k.state FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.id=$1 AND k.endpoint_id=$2 FOR SHARE OF n,k`, node, endpoint).Scan(&v.WorkspaceID, &v.NodeStatus, &v.EndpointState)
	return
}
func (s transportIngressStore) InsertEvent(ctx context.Context, v transportstore.TransportEvent) (bool, error) {
	n, err := s.Exec(ctx, `INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload)VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(event_id) DO NOTHING`, v.ID, v.NodeID, v.Type, v.At, v.Traceparent, v.Payload)
	return n != 0, err
}
func (s transportIngressStore) NodeStatus(ctx context.Context, node uuid.UUID, state string, at time.Time) error {
	_, err := s.Exec(ctx, `UPDATE nodes SET status=$2,updated_at=$3,version=version+1 WHERE id=$1 AND status IN ('active','offline') AND status IS DISTINCT FROM $2`, node, state, at)
	return err
}
func (s transportIngressStore) SimulationOutcome(ctx context.Context, node uuid.UUID, state, traceparent string, disconnected bool, at time.Time) error {
	if disconnected {
		_, err := s.Exec(ctx, `UPDATE operations o SET state='unknown',updated_at=$2,version=version+1 FROM local_slice_jobs j WHERE o.id=j.operation_id AND o.node_id=$1 AND j.dispatched_at IS NOT NULL AND o.state NOT IN ('succeeded','failed','expired','rolled_back','superseded')`, node, at)
		return err
	}
	_, err := s.Exec(ctx, `UPDATE operations o SET state=$2,updated_at=$3,version=version+1 FROM local_slice_jobs j WHERE o.id=j.operation_id AND o.node_id=$1 AND j.traceparent=$4 AND o.state NOT IN ('succeeded','failed','expired','rolled_back','superseded')`, node, state, at, traceparent)
	return err
}
func (s transportIngressStore) InsertQuarantine(ctx context.Context, v transportstore.Quarantine) (bool, error) {
	n, err := s.Exec(ctx, `INSERT INTO transport_event_quarantine(event_id,node_id,event_type,payload_sha256,reason_code,reason_detail,observed_at)VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(event_id) DO NOTHING`, v.EventID, v.NodeID, v.Type, v.PayloadHash, v.ReasonCode, v.ReasonDetail, v.At)
	return n != 0, err
}
func (s transportIngressStore) Quarantine(ctx context.Context, id uuid.UUID) (v transportstore.Quarantine, err error) {
	err = s.QueryRow(ctx, `SELECT node_id,event_type,payload_sha256 FROM transport_event_quarantine WHERE event_id=$1`, id).Scan(&v.NodeID, &v.Type, &v.PayloadHash)
	return
}
func (s transportIngressStore) QuarantineAlert(ctx context.Context, id, workspace uuid.UUID, v transportstore.Quarantine) error {
	_, err := s.Exec(ctx, `INSERT INTO security_alerts(id,workspace_id,severity,kind,node_id,resource_type,resource_id,created_at)VALUES($1,$2,'high','transport_event.permanent_invalid',$3,'transport_event',$4,$5)`, id, workspace, v.NodeID, v.EventID, v.At)
	return err
}
func (s transportIngressStore) AdvanceCursor(ctx context.Context, event uuid.UUID, at time.Time) error {
	_, err := s.Exec(ctx, `INSERT INTO transport_event_cursor(singleton,event_id,valid,updated_at)VALUES(true,$1,true,$2) ON CONFLICT(singleton) DO UPDATE SET event_id=EXCLUDED.event_id,valid=true,updated_at=EXCLUDED.updated_at`, event, at)
	return err
}
func (s transportIngressStore) LastEventID(ctx context.Context) (id uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT event_id FROM transport_event_cursor WHERE singleton AND valid`).Scan(&id)
	return
}
