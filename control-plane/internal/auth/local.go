package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

const LocalIssuer = "local"

func (s *Service) HasLocalCredential(ctx context.Context, id uuid.UUID) (bool, error) {
	if !s.localEnabled {
		return false, ErrLocalDisabled
	}
	return authstore.HasLocalCredential(ctx, s.backend, id)
}

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
	id := uuid.Must(uuid.NewV7())
	err = s.withAuthentication(ctx, func(_ database.Tx, store authstore.Store) error {
		return store.InsertCredential(ctx, id, username, hash, s.now())
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func (s *Service) localCredential(ctx context.Context, username string) (localCredential, error) {
	c, err := authstore.ReadCredential(ctx, s.backend, username)
	return localCredential{identityID: c.ID, passwordHash: c.Hash, disabled: c.Disabled}, err
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
	if err != nil && !errors.Is(err, database.ErrNotFound) {
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
