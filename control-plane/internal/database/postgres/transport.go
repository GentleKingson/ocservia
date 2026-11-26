package postgres

import (
	"context"
	"time"

	transportstore "github.com/GentleKingson/ocservia/control-plane/internal/transportevents/store"
	"github.com/google/uuid"
)

type transportEventStore struct{ *transaction }

func (t *transaction) TransportEventStore() transportstore.Store { return transportEventStore{t} }

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

func (s transportEventStore) InvalidateCursor(ctx context.Context) error {
	if _, err := s.Exec(ctx, `UPDATE transport_events SET transport_cursor_valid=false WHERE transport_cursor_valid`); err != nil {
		return err
	}
	_, err := s.Exec(ctx, `UPDATE transport_event_cursor SET valid=false,updated_at=now() WHERE singleton`)
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
