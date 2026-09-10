package postgres

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	transportstore "github.com/GentleKingson/ocservia/control-plane/internal/localslice/store"
	"github.com/google/uuid"
)

type transportIngressStore struct{ database.Tx }

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
	err = s.QueryRow(ctx, `SELECT n.workspace_id,n.status,k.state FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.id=$1 AND k.endpoint_id=$2 FOR UPDATE OF n,k`, node, endpoint).Scan(&v.WorkspaceID, &v.NodeStatus, &v.EndpointState)
	return
}
func (s transportIngressStore) InsertEvent(ctx context.Context, v transportstore.TransportEvent) (bool, error) {
	n, err := s.Exec(ctx, `INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload)VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(event_id) DO NOTHING`, v.ID, v.NodeID, v.Type, v.At, v.Traceparent, v.Payload)
	return n != 0, err
}
func (s transportIngressStore) NodeStatus(ctx context.Context, node uuid.UUID, state string, at value.Timestamp) error {
	_, err := s.Exec(ctx, `UPDATE nodes SET status=$2,updated_at=$3,version=version+1 WHERE id=$1 AND status IN ('active','offline') AND status IS DISTINCT FROM $2`, node, state, at)
	return err
}
func (s transportIngressStore) SimulationOutcome(ctx context.Context, node uuid.UUID, state, traceparent string, disconnected bool, at value.Timestamp) error {
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
func (s transportIngressStore) AdvanceCursor(ctx context.Context, event uuid.UUID, at value.Timestamp) error {
	_, err := s.Exec(ctx, `INSERT INTO transport_event_cursor(singleton,event_id,valid,updated_at)VALUES(true,$1,true,$2) ON CONFLICT(singleton) DO UPDATE SET event_id=EXCLUDED.event_id,valid=true,updated_at=EXCLUDED.updated_at`, event, at)
	return err
}
func (s transportIngressStore) LastEventID(ctx context.Context) (id uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT event_id FROM transport_event_cursor WHERE singleton AND valid`).Scan(&id)
	return
}
