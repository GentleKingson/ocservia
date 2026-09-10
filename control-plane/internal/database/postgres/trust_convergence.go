package postgres

import (
	"context"
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
	err = s.QueryRow(ctx, `SELECT node_id,endpoint_id,desired_state,revision,reason,update_applied,close_required,close_applied,attempts
		FROM node_trust_convergence WHERE (NOT update_applied OR (close_required AND NOT close_applied))
		AND available_at<=now() AND (locked_until IS NULL OR locked_until<now())
		ORDER BY available_at,node_id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(
		&v.NodeID, &v.EndpointID, &v.DesiredState, &v.Revision, &v.Reason, &v.UpdateApplied, &v.CloseRequired, &v.CloseApplied, &v.Attempts)
	if err != nil {
		return v, err
	}
	n, err := s.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=$2,locked_until=now()+interval '10 seconds',attempts=attempts+1,updated_at=now() WHERE node_id=$1 AND revision=$3`, v.NodeID, worker, v.Revision)
	if err == nil && n != 1 {
		err = database.ErrNotFound
	}
	v.Attempts++
	return v, err
}

func (s trustConvergenceStore) MarkUpdateApplied(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE node_trust_convergence SET update_applied=true,locked_until=now()+interval '10 seconds',last_error=NULL,updated_at=now() WHERE node_id=$1 AND revision=$2 AND locked_by=$3`, v.NodeID, v.Revision, worker)
	return n == 1, err
}

func (s trustConvergenceStore) MarkCloseApplied(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE node_trust_convergence SET close_applied=true,locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=now() WHERE node_id=$1 AND revision=$2 AND locked_by=$3`, v.NodeID, v.Revision, worker)
	return n == 1, err
}

func (s trustConvergenceStore) UnlockComplete(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) error {
	_, err := s.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=now() WHERE node_id=$1 AND revision=$2 AND locked_by=$3`, v.NodeID, v.Revision, worker)
	return err
}

func (s trustConvergenceStore) Release(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID, delay time.Duration, detail string) error {
	_, err := s.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=NULL,locked_until=NULL,available_at=now()+$4::interval,last_error=$5,updated_at=now() WHERE node_id=$1 AND revision=$2 AND locked_by=$3`, v.NodeID, v.Revision, worker, delay.String(), detail)
	return err
}
