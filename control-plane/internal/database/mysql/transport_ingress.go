package mysql

import (
	"context"
	"errors"

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
	err = s.QueryRow(ctx, `SELECT n.workspace_id,n.status,k.state FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.id=? AND k.endpoint_id=? FOR UPDATE`, UUIDBytes(node), endpoint).Scan(&v.WorkspaceID, &v.NodeStatus, &v.EndpointState)
	return
}
func (s transportIngressStore) InsertEvent(ctx context.Context, v transportstore.TransportEvent) (bool, error) {
	_, err := s.Exec(ctx, `INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload)VALUES(?,?,?,?,?,?)`, UUIDBytes(v.ID), UUIDBytes(v.NodeID), v.Type, v.At, v.Traceparent, v.Payload)
	// Duplicate-key errors roll back only this statement in InnoDB. Avoid a
	// no-op UPDATE: the runtime principal cannot mutate event identity.
	if errors.Is(err, database.ErrUnique) {
		return false, nil
	}
	return err == nil, err
}
func (s transportIngressStore) NodeStatus(ctx context.Context, node uuid.UUID, state string, at value.Timestamp) error {
	_, err := s.Exec(ctx, `UPDATE nodes SET status=?,updated_at=?,version=version+1 WHERE id=? AND status IN ('active','offline') AND status<>?`, state, at, UUIDBytes(node), state)
	return err
}
func (s transportIngressStore) SimulationOutcome(ctx context.Context, node uuid.UUID, state, traceparent string, disconnected bool, at value.Timestamp) error {
	if disconnected {
		_, err := s.Exec(ctx, `UPDATE operations o JOIN local_slice_jobs j ON j.operation_id=o.id SET o.state='unknown',o.updated_at=?,o.version=o.version+1 WHERE o.node_id=? AND j.dispatched_at IS NOT NULL AND o.state NOT IN ('succeeded','failed','expired','rolled_back','superseded')`, at, UUIDBytes(node))
		return err
	}
	_, err := s.Exec(ctx, `UPDATE operations o JOIN local_slice_jobs j ON j.operation_id=o.id SET o.state=?,o.updated_at=?,o.version=o.version+1 WHERE o.node_id=? AND BINARY j.traceparent=BINARY ? AND o.state NOT IN ('succeeded','failed','expired','rolled_back','superseded')`, state, at, UUIDBytes(node), traceparent)
	return err
}
func (s transportIngressStore) InsertQuarantine(ctx context.Context, v transportstore.Quarantine) (bool, error) {
	_, err := s.Exec(ctx, `INSERT INTO transport_event_quarantine(event_id,node_id,event_type,payload_sha256,reason_code,reason_detail,observed_at)VALUES(?,?,?,?,?,?,?)`, UUIDBytes(v.EventID), UUIDBytes(v.NodeID), v.Type, v.PayloadHash, v.ReasonCode, v.ReasonDetail, v.At)
	if errors.Is(err, database.ErrUnique) {
		return false, nil
	}
	return err == nil, err
}
func (s transportIngressStore) Quarantine(ctx context.Context, id uuid.UUID) (v transportstore.Quarantine, err error) {
	err = s.QueryRow(ctx, `SELECT node_id,event_type,payload_sha256 FROM transport_event_quarantine WHERE event_id=?`, UUIDBytes(id)).Scan(&v.NodeID, &v.Type, &v.PayloadHash)
	return
}
func (s transportIngressStore) QuarantineAlert(ctx context.Context, id, workspace uuid.UUID, v transportstore.Quarantine) error {
	_, err := s.Exec(ctx, `INSERT INTO security_alerts(id,workspace_id,severity,kind,node_id,resource_type,resource_id,created_at)VALUES(?,?,'high','transport_event.permanent_invalid',?,'transport_event',?,?)`, UUIDBytes(id), UUIDBytes(workspace), UUIDBytes(v.NodeID), UUIDBytes(v.EventID), v.At)
	return err
}
func (s transportIngressStore) AdvanceCursor(ctx context.Context, event uuid.UUID, at value.Timestamp) error {
	_, err := s.Exec(ctx, `INSERT INTO transport_event_cursor(singleton,event_id,valid,updated_at)VALUES(true,?,true,?) ON DUPLICATE KEY UPDATE event_id=VALUES(event_id),valid=true,updated_at=VALUES(updated_at)`, UUIDBytes(event), at)
	return err
}
func (s transportIngressStore) LastEventID(ctx context.Context) (id uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT event_id FROM transport_event_cursor WHERE singleton AND valid`).Scan(&id)
	return
}
