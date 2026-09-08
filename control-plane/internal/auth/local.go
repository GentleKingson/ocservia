package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const LocalIssuer = "local"

type localCredential struct {
	identityID   uuid.UUID
	passwordHash string
	disabled     bool
	attemptLease uuid.UUID
}

// CreateLocalCredential is a provisioning primitive for trusted callers, not
// self-registration. It grants no roles and never links an existing identity.
func (s *Service) CreateLocalCredential(ctx context.Context, username, password string) (uuid.UUID, error) {
	if !s.localEnabled {
		return uuid.Nil, ErrLocalDisabled
	}
	username, err := normalizeLocalUsername(username)
	if err != nil {
		return uuid.Nil, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return uuid.Nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	id, err := s.insertLocalCredential(ctx, tx, username, hash)
	if err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func (s *Service) insertLocalCredential(ctx context.Context, tx pgx.Tx, username, hash string) (uuid.UUID, error) {
	id := uuid.Must(uuid.NewV7())
	now := s.now()
	if _, err := tx.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,$2,$3,$4,$4)`, id, LocalIssuer, username, now); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO local_credentials(identity_id,username,password_hash,created_at,updated_at,password_changed_at) VALUES($1,$2,$3,$4,$4,$4)`, id, username, hash, now); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func (s *Service) localCredential(ctx context.Context, username string) (localCredential, error) {
	var credential localCredential
	err := s.pool.QueryRow(ctx, `SELECT i.id,c.password_hash,i.disabled_at IS NOT NULL FROM local_credentials c JOIN identities i ON i.id=c.identity_id WHERE c.username=$1 AND i.issuer=$2 AND i.subject=c.username`, username, LocalIssuer).Scan(&credential.identityID, &credential.passwordHash, &credential.disabled)
	return credential, err
}

// AuthenticateLocal returns the same session cookie and principal as OIDC.
func (s *Service) AuthenticateLocal(ctx context.Context, username, password string) (cookie *http.Cookie, principal Principal, resultErr error) {
	if !s.localEnabled {
		return nil, Principal{}, ErrLocalDisabled
	}
	username, err := normalizeLocalUsername(username)
	if err != nil || len(password) == 0 || len(password) > maxPasswordBytes {
		return nil, Principal{}, ErrUnauthenticated
	}
	lease, err := s.reserveLocalAttempt(ctx, username)
	if err != nil {
		return nil, Principal{}, err
	}
	failed := false
	defer func() {
		if cookie != nil {
			return // Session commit already cleared the lease and failures.
		}
		if err := s.finishLocalAttempt(username, lease, failed); err != nil {
			cookie, principal, resultErr = nil, Principal{}, err
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, localAttemptLease)
	defer cancel()
	credential, err := s.localCredential(ctx, username)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, Principal{}, err
	}
	found := err == nil
	hash := credential.passwordHash
	if !found {
		hash = dummyPasswordHash
	}
	valid, err := verifyPassword(hash, password)
	if err != nil {
		_, _ = verifyPassword(dummyPasswordHash, password)
		return nil, Principal{}, err
	}
	if !found || !valid {
		failed = true
		return nil, Principal{}, ErrUnauthenticated
	}
	if credential.disabled {
		return nil, Principal{}, ErrUnauthenticated
	}
	credential.attemptLease = lease
	return s.createSession(ctx, LocalIssuer, username, "", "", false, &credential)
}
