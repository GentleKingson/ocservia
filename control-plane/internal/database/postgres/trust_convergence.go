package postgres

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
	_, err := s.Exec(ctx, `INSERT INTO node_trust_convergence
		(node_id,endpoint_id,desired_state,revision,reason,close_required,available_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$3::text='revoked',$6,$6,$6)
		ON CONFLICT(node_id) DO UPDATE SET endpoint_id=EXCLUDED.endpoint_id,desired_state=EXCLUDED.desired_state,
		revision=EXCLUDED.revision,reason=EXCLUDED.reason,update_applied=false,close_required=EXCLUDED.close_required,
		close_applied=false,available_at=EXCLUDED.available_at,locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=EXCLUDED.updated_at
		WHERE node_trust_convergence.revision < EXCLUDED.revision`, v.NodeID, v.EndpointID, v.DesiredState, v.Revision, v.Reason, at)
	return err
}

func (s trustConvergenceStore) Claim(ctx context.Context, worker uuid.UUID) (v enrollmentstore.TrustJob, err error) {
	at, err := database.WallTime(ctx, s.Tx)
	if err != nil {
		return v, err
	}
	err = s.QueryRow(ctx, `SELECT node_id,endpoint_id,desired_state,revision,reason,update_applied,close_required,close_applied,attempts
		FROM node_trust_convergence WHERE (NOT update_applied OR (close_required AND NOT close_applied))
		AND available_at<=$1 AND (locked_until IS NULL OR locked_until<=$1)
		ORDER BY available_at,node_id FOR UPDATE SKIP LOCKED LIMIT 1`, at).Scan(
		&v.NodeID, &v.EndpointID, &v.DesiredState, &v.Revision, &v.Reason, &v.UpdateApplied, &v.CloseRequired, &v.CloseApplied, &v.Attempts)
	if err != nil {
		return v, err
	}
	at, err = database.WallTime(ctx, s.Tx)
	if err != nil {
		return v, err
	}
	until, err := at.Add(enrollmentstore.TrustLeaseTTL)
	if err != nil {
		return v, err
	}
	n, err := s.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=$2,locked_until=$4,attempts=attempts+1,updated_at=$5 WHERE node_id=$1 AND revision=$3`, v.NodeID, worker, v.Revision, until, at)
	if err == nil && n != 1 {
		err = database.ErrNotFound
	}
	v.Attempts++
	return v, err
}

// Check time after the row lock, including waits that cross the lease deadline.
func (s trustConvergenceStore) lockClaim(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) (value.Timestamp, bool, error) {
	var until value.Timestamp
	err := s.QueryRow(ctx, `SELECT locked_until FROM node_trust_convergence WHERE node_id=$1 AND revision=$2 AND locked_by=$3 AND attempts=$4 FOR UPDATE`, v.NodeID, v.Revision, worker, v.Attempts).Scan(&until)
	if errors.Is(err, database.ErrNotFound) {
		return value.Timestamp{}, false, nil
	}
	if err != nil {
		return value.Timestamp{}, false, err
	}
	at, err := database.WallTime(ctx, s.Tx)
	return at, err == nil && until.Valid && until.Micros > at.Micros, err
}

func (s trustConvergenceStore) Renew(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) (bool, error) {
	at, valid, err := s.lockClaim(ctx, v, worker)
	if err != nil || !valid {
		return false, err
	}
	until, err := at.Add(enrollmentstore.TrustLeaseTTL)
	if err != nil {
		return false, err
	}
	_, err = s.Exec(ctx, `UPDATE node_trust_convergence SET locked_until=$2,updated_at=$3 WHERE node_id=$1`, v.NodeID, until, at)
	return err == nil, err
}

func (s trustConvergenceStore) MarkUpdateApplied(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) (bool, error) {
	at, valid, err := s.lockClaim(ctx, v, worker)
	if err != nil || !valid {
		return false, err
	}
	until, err := at.Add(enrollmentstore.TrustLeaseTTL)
	if err != nil {
		return false, err
	}
	_, err = s.Exec(ctx, `UPDATE node_trust_convergence SET update_applied=true,locked_until=$2,last_error=NULL,updated_at=$3 WHERE node_id=$1`, v.NodeID, until, at)
	return err == nil, err
}

func (s trustConvergenceStore) MarkCloseApplied(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) (bool, error) {
	at, valid, err := s.lockClaim(ctx, v, worker)
	if err != nil || !valid {
		return false, err
	}
	n, err := s.Exec(ctx, `UPDATE node_trust_convergence SET close_applied=true,locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=$2 WHERE node_id=$1 AND update_applied AND close_required`, v.NodeID, at)
	return n == 1, err
}

func (s trustConvergenceStore) UnlockComplete(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) error {
	at, valid, err := s.lockClaim(ctx, v, worker)
	if err != nil || !valid {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=$2 WHERE node_id=$1 AND update_applied AND (NOT close_required OR close_applied)`, v.NodeID, at)
	return err
}

func (s trustConvergenceStore) Release(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID, delay time.Duration, detail string) error {
	at, valid, err := s.lockClaim(ctx, v, worker)
	if err != nil || !valid {
		return err
	}
	next, err := at.Add(delay)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=NULL,locked_until=NULL,available_at=$2,last_error=$3,updated_at=$4 WHERE node_id=$1`, v.NodeID, next, detail, at)
	return err
}
