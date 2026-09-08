package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	localAttemptCapacity        = 16384
	localAttemptWindow          = 15 * time.Minute
	localAttemptLease           = 30 * time.Second
	localAttemptDBTimeout       = 2 * time.Second
	localAttemptLockID    int64 = 734821033
)

var errLocalAttemptCapacity = errors.New("Local authentication attempt capacity exhausted")

// reserveLocalAttempt receives only normalizeLocalUsername output. A durable
// single-flight lease prevents concurrent KDF work from overshooting a threshold.
// The capacity lock and row locks are released before credential lookup or KDF.
func (s *Service) reserveLocalAttempt(ctx context.Context, username string) (uuid.UUID, error) {
	ctx, cancel := context.WithTimeout(ctx, localAttemptDBTimeout)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, localAttemptLockID); err != nil {
		return uuid.Nil, err
	}
	// Admission drives indexed expiry cleanup, even at capacity. Never evict a
	// live account to make room for attacker-selected random usernames.
	if _, err := tx.Exec(ctx, `DELETE FROM local_auth_attempts WHERE expires_at <= statement_timestamp()`); err != nil {
		return uuid.Nil, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM local_auth_attempts WHERE username=$1)`, username).Scan(&exists); err != nil {
		return uuid.Nil, err
	}
	if !exists {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM local_auth_attempts`).Scan(&count); err != nil {
			return uuid.Nil, err
		}
		if count >= localAttemptCapacity {
			return uuid.Nil, errLocalAttemptCapacity
		}
		if _, err := tx.Exec(ctx, `INSERT INTO local_auth_attempts(username,window_until,expires_at) VALUES($1,clock_timestamp()+$2::interval,clock_timestamp()+$2::interval)`, username, localAttemptWindow.String()); err != nil {
			return uuid.Nil, err
		}
	}
	lease := uuid.New()
	result, err := tx.Exec(ctx, `UPDATE local_auth_attempts SET
		failures=CASE WHEN window_until<=clock_timestamp() THEN 0 ELSE failures END,
		window_until=CASE WHEN window_until<=clock_timestamp() THEN clock_timestamp()+$3::interval ELSE window_until END,
		lease_id=$2, lease_until=clock_timestamp()+$4::interval,
		expires_at=GREATEST(CASE WHEN window_until<=clock_timestamp() THEN clock_timestamp()+$3::interval ELSE window_until END,clock_timestamp()+$4::interval)
		WHERE username=$1 AND blocked_until<=clock_timestamp() AND lease_until<=clock_timestamp()`,
		username, lease, localAttemptWindow.String(), localAttemptLease.String())
	if err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	if result.RowsAffected() != 1 {
		return uuid.Nil, ErrUnauthenticated
	}
	return lease, nil
}

// Completion deliberately survives request cancellation. Only an observed bad
// password is counted; cancellation, DB/hash errors and stale credentials release
// the lease without a failure. If completion fails/crashes, the lease expires.
func (s *Service) finishLocalAttempt(username string, lease uuid.UUID, failed bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), localAttemptDBTimeout)
	defer cancel()
	_, err := s.pool.Exec(ctx, `UPDATE local_auth_attempts SET
		failures=CASE WHEN $3 THEN LEAST(failures+1,14) ELSE failures END,
		blocked_until=CASE WHEN $3 AND failures>=4
			THEN clock_timestamp()+LEAST(300,power(2,LEAST(failures-4,9))) * interval '1 second'
			ELSE blocked_until END,
		expires_at=GREATEST(window_until,CASE WHEN $3 AND failures>=4
			THEN clock_timestamp()+LEAST(300,power(2,LEAST(failures-4,9))) * interval '1 second'
			ELSE blocked_until END),
		lease_id=NULL, lease_until='-infinity'
		WHERE username=$1 AND lease_id=$2 AND lease_until>clock_timestamp()`, username, lease, failed)
	return err
}

// Called after locking and rechecking the credential, in the session transaction.
// Token fencing prevents a late success from deleting a replacement lease/state.
func clearLocalAttempt(ctx context.Context, tx pgx.Tx, username string, lease uuid.UUID) error {
	result, err := tx.Exec(ctx, `DELETE FROM local_auth_attempts WHERE username=$1 AND lease_id=$2 AND lease_until>clock_timestamp()`, username, lease)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrUnauthenticated
	}
	return nil
}
