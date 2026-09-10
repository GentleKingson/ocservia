package mysql

import (
	"context"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/google/uuid"
)

type trustConvergenceStore struct{ database.Tx }

func (t *transaction) TrustConvergenceStore() enrollmentstore.TrustStore {
	return trustConvergenceStore{t}
}

func (s trustConvergenceStore) Enqueue(ctx context.Context, v enrollmentstore.TrustJob, at value.Timestamp) error {
	// The node lock also serializes first insertion, when no queue row exists.
	var node uuid.UUID
	if err := s.QueryRow(ctx, `SELECT id FROM nodes WHERE id=? FOR UPDATE`, UUIDBytes(v.NodeID)).Scan(&node); err != nil {
		return err
	}
	var revision uint64
	err := s.QueryRow(ctx, `SELECT revision FROM node_trust_convergence WHERE node_id=? FOR UPDATE`, UUIDBytes(v.NodeID)).Scan(&revision)
	if errors.Is(err, database.ErrNotFound) {
		_, err = s.Exec(ctx, `INSERT INTO node_trust_convergence(node_id,endpoint_id,desired_state,revision,reason,close_required,available_at,created_at,updated_at)VALUES(?,?,?,?,?,?,?,?,?)`, UUIDBytes(v.NodeID), v.EndpointID, v.DesiredState, v.Revision, v.Reason, v.DesiredState == "revoked", at, at, at)
		return err
	}
	if err != nil || revision >= v.Revision {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE node_trust_convergence SET endpoint_id=?,desired_state=?,revision=?,reason=?,update_applied=false,close_required=?,close_applied=false,available_at=?,locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=? WHERE node_id=?`, v.EndpointID, v.DesiredState, v.Revision, v.Reason, v.DesiredState == "revoked", at, at, UUIDBytes(v.NodeID))
	return err
}

func (s trustConvergenceStore) Claim(ctx context.Context, worker uuid.UUID) (v enrollmentstore.TrustJob, err error) {
	at, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return v, err
	}
	until, err := at.Add(10 * time.Second)
	if err != nil {
		return v, err
	}
	err = s.QueryRow(ctx, `SELECT node_id,endpoint_id,desired_state,revision,reason,update_applied,close_required,close_applied,attempts FROM node_trust_convergence WHERE (NOT update_applied OR (close_required AND NOT close_applied)) AND available_at<=? AND (locked_until IS NULL OR locked_until<?) ORDER BY available_at,node_id LIMIT 1 FOR UPDATE SKIP LOCKED`, at, at).Scan(
		&v.NodeID, &v.EndpointID, &v.DesiredState, &v.Revision, &v.Reason, &v.UpdateApplied, &v.CloseRequired, &v.CloseApplied, &v.Attempts)
	if err != nil {
		return v, err
	}
	n, err := s.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=?,locked_until=?,attempts=attempts+1,updated_at=? WHERE node_id=? AND revision=?`, UUIDBytes(worker), until, at, UUIDBytes(v.NodeID), v.Revision)
	if err == nil && n != 1 {
		err = database.ErrNotFound
	}
	v.Attempts++
	return v, err
}

func (s trustConvergenceStore) MarkUpdateApplied(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) (bool, error) {
	at, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return false, err
	}
	until, err := at.Add(10 * time.Second)
	if err != nil {
		return false, err
	}
	// PostgreSQL reports matched rows even if a repeated mark changes no value.
	var node uuid.UUID
	err = s.QueryRow(ctx, `SELECT node_id FROM node_trust_convergence WHERE node_id=? AND revision=? AND locked_by=? FOR UPDATE`, UUIDBytes(v.NodeID), v.Revision, UUIDBytes(worker)).Scan(&node)
	if errors.Is(err, database.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = s.Exec(ctx, `UPDATE node_trust_convergence SET update_applied=true,locked_until=?,last_error=NULL,updated_at=? WHERE node_id=? AND revision=? AND locked_by=?`, until, at, UUIDBytes(v.NodeID), v.Revision, UUIDBytes(worker))
	return err == nil, err
}

func (s trustConvergenceStore) MarkCloseApplied(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) (bool, error) {
	at, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return false, err
	}
	n, err := s.Exec(ctx, `UPDATE node_trust_convergence SET close_applied=true,locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=? WHERE node_id=? AND revision=? AND locked_by=?`, at, UUIDBytes(v.NodeID), v.Revision, UUIDBytes(worker))
	return n == 1, err
}

func (s trustConvergenceStore) UnlockComplete(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) error {
	at, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=? WHERE node_id=? AND revision=? AND locked_by=?`, at, UUIDBytes(v.NodeID), v.Revision, UUIDBytes(worker))
	return err
}

func (s trustConvergenceStore) Release(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID, delay time.Duration, detail string) error {
	at, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	next, err := at.Add(delay)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=NULL,locked_until=NULL,available_at=?,last_error=?,updated_at=? WHERE node_id=? AND revision=? AND locked_by=?`, next, detail, at, UUIDBytes(v.NodeID), v.Revision, UUIDBytes(worker))
	return err
}
