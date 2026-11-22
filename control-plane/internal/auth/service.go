package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

const SessionCookieName = "__Host-ocservia_session"

var (
	ErrUnauthenticated       = errors.New("principal is not authenticated")
	ErrForbidden             = errors.New("authorization denied")
	ErrLocalDisabled         = errors.New("local authentication is disabled")
	ErrBreakGlassDisabled    = errors.New("break-glass is disabled")
	ErrBreakGlassRotationDue = errors.New("break-glass credential rotation is required")
)

type Config struct {
	LocalEnabled        bool
	SessionKey          []byte
	SessionTTL          time.Duration
	BreakGlassEnabled   bool
	BreakGlassTokenHash []byte
}

type Service struct {
	backend             database.Backend
	localEnabled        bool
	aead                cipher.AEAD
	accountLogKey       []byte
	sessionTTL          time.Duration
	breakGlassEnabled   bool
	breakGlassTokenHash []byte
	now                 func() time.Time
	random              io.Reader
}

type Principal struct {
	IdentityID uuid.UUID
	SessionID  uuid.UUID
	Subject    string
	Issuer     string
	BreakGlass bool
	ExpiresAt  time.Time
}

type sessionEnvelope struct {
	SessionID, IdentityID string
	ExpiresAt             time.Time
}

// NewBackend constructs a Local authentication and single-admin workspace authorization service.
func NewBackend(backend database.Backend, cfg Config) (*Service, error) {
	if backend == nil || len(cfg.SessionKey) != 32 || cfg.SessionTTL < time.Minute || cfg.SessionTTL > 24*time.Hour {
		return nil, errors.New("invalid authentication configuration")
	}
	block, err := aes.NewCipher(cfg.SessionKey)
	if err != nil {
		return nil, fmt.Errorf("configure session encryption: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("configure session encryption: %w", err)
	}
	return &Service{
		localEnabled:        cfg.LocalEnabled,
		accountLogKey:       deriveAccountLogKey(cfg.SessionKey),
		backend:             backend,
		aead:                aead,
		sessionTTL:          cfg.SessionTTL,
		breakGlassEnabled:   cfg.BreakGlassEnabled,
		breakGlassTokenHash: cfg.BreakGlassTokenHash,
		now:                 func() time.Time { return time.Now().UTC() },
		random:              rand.Reader,
	}, nil
}

func (s *Service) LocalEnabled() bool { return s.localEnabled }

func (s *Service) Authenticate(ctx context.Context, cookie *http.Cookie) (Principal, error) {
	var envelope sessionEnvelope
	if cookie == nil || s.open(cookie.Value, &envelope) != nil || s.now().After(envelope.ExpiresAt) {
		return Principal{}, ErrUnauthenticated
	}
	sessionID, err := uuid.Parse(envelope.SessionID)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	identityID, err := uuid.Parse(envelope.IdentityID)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	principal := Principal{SessionID: sessionID, IdentityID: identityID}
	err = s.withAuthentication(ctx, func(_ database.Tx, store authstore.Store) error {
		session, err := store.Session(ctx, sessionID, identityID)
		if err != nil {
			return err
		}
		principal.Issuer, principal.Subject, principal.BreakGlass = session.Issuer, session.Subject, session.BreakGlass
		principal.ExpiresAt = envelope.ExpiresAt
		if session.ExpiresAt.IsZero() {
			return ErrUnauthenticated
		}
		if session.ExpiresAt.Before(principal.ExpiresAt) {
			principal.ExpiresAt = session.ExpiresAt
		}
		return nil
	})
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	if envelope.ExpiresAt.Before(principal.ExpiresAt) {
		principal.ExpiresAt = envelope.ExpiresAt
	}
	return principal, nil
}

func (s *Service) Logout(ctx context.Context, principal Principal) error {
	return s.withAuthentication(ctx, func(_ database.Tx, store authstore.Store) error {
		return store.RevokeSession(ctx, principal.SessionID, principal.IdentityID)
	})
}

// ValidBreakGlassToken is only an admission hint, not session authorization.
// BreakGlass must still enforce rotation and commit the audited session.
func (s *Service) ValidBreakGlassToken(token string) bool {
	if !s.breakGlassEnabled || len(s.breakGlassTokenHash) != sha256.Size {
		return false
	}
	digest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(digest[:], s.breakGlassTokenHash) == 1
}

func (s *Service) BreakGlass(ctx context.Context, token string, requestID string) (*http.Cookie, Principal, error) {
	if !s.breakGlassEnabled || len(s.breakGlassTokenHash) != sha256.Size {
		return nil, Principal{}, ErrBreakGlassDisabled
	}
	if !s.ValidBreakGlassToken(token) {
		return nil, Principal{}, ErrUnauthenticated
	}
	fingerprint := sha256.Sum256(s.breakGlassTokenHash)
	identityID := uuid.Must(uuid.NewV7())
	sessionID := uuid.Must(uuid.NewV7())
	var expires time.Time
	err := s.withAuthentication(ctx, func(tx database.Tx, store authstore.Store) error {
		used, err := store.BreakGlassUsed(ctx, fingerprint[:])
		if err != nil {
			return err
		}
		if used {
			return ErrBreakGlassRotationDue
		}
		now := s.now()
		expires = now.Add(15 * time.Minute)
		identityID, err = store.BreakGlassIdentity(ctx, identityID, now)
		if err != nil {
			return err
		}
		if err = store.InsertSession(ctx, sessionID, identityID, expires, true, now); err != nil {
			return err
		}
		if err = store.RecordBreakGlass(ctx, fingerprint[:], identityID, sessionID, uuid.Must(uuid.NewV7()), now); err != nil {
			return err
		}
		workspaces, err := store.Workspaces(ctx)
		if err != nil {
			return err
		}
		for _, workspaceID := range workspaces {
			if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "break_glass", ActorID: identityID.String(), SessionID: &sessionID, Action: "break_glass.use", ResourceType: "platform", RequestID: requestID, Result: "succeeded", Reason: "emergency offline access", At: now}); err != nil {
				return fmt.Errorf("append break-glass audit: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, Principal{}, err
	}
	value, err := s.seal(sessionEnvelope{SessionID: sessionID.String(), IdentityID: identityID.String(), ExpiresAt: expires})
	if err != nil {
		return nil, Principal{}, err
	}
	return secureCookie(SessionCookieName, value, expires), Principal{IdentityID: identityID, SessionID: sessionID, Subject: "offline", Issuer: "break-glass", BreakGlass: true, ExpiresAt: expires}, nil
}

func (s *Service) createSession(ctx context.Context, issuer, subject string, local *localCredential) (*http.Cookie, Principal, error) {
	sessionID := uuid.Must(uuid.NewV7())
	var identityID uuid.UUID
	var expires time.Time
	err := s.withAuthentication(ctx, func(_ database.Tx, store authstore.Store) error {
		if local == nil {
			return ErrUnauthenticated
		}
		now := s.now()
		expires = now.Add(s.sessionTTL)
		var err error
		identityID, err = store.LockCredential(ctx, local.identityID, issuer, subject, local.passwordHash)
		if errors.Is(err, database.ErrNotFound) {
			return ErrUnauthenticated
		}
		if err != nil {
			return err
		}
		cleared, err := store.ClearAttempt(ctx, subject, local.attemptLease)
		if err != nil {
			return err
		}
		if !cleared {
			return ErrUnauthenticated
		}
		return store.InsertSession(ctx, sessionID, identityID, expires, false, now)
	})
	if err != nil {
		return nil, Principal{}, err
	}
	value, err := s.seal(sessionEnvelope{SessionID: sessionID.String(), IdentityID: identityID.String(), ExpiresAt: expires})
	if err != nil {
		return nil, Principal{}, err
	}
	return secureCookie(SessionCookieName, value, expires), Principal{IdentityID: identityID, SessionID: sessionID, Subject: subject, Issuer: issuer, BreakGlass: false, ExpiresAt: expires}, nil
}

func (s *Service) withAuthentication(ctx context.Context, change func(database.Tx, authstore.Store) error) error {
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := authstore.From(tx)
		if err != nil {
			return err
		}
		return change(tx, store)
	})
}

func (s *Service) seal(value any) (string, error) {
	plaintext, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(s.random, nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(append(nonce, s.aead.Seal(nil, nonce, plaintext, nil)...)), nil
}

func (s *Service) open(value string, target any) error {
	sealed, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(sealed) <= s.aead.NonceSize() {
		return ErrUnauthenticated
	}
	nonce := sealed[:s.aead.NonceSize()]
	plaintext, err := s.aead.Open(nil, nonce, sealed[s.aead.NonceSize():], nil)
	if err != nil {
		return ErrUnauthenticated
	}
	return json.Unmarshal(plaintext, target)
}

func secureCookie(name, value string, expires time.Time) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: int(time.Until(expires).Seconds())}
}

func ClearCookie(name string) *http.Cookie {
	return &http.Cookie{Name: name, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)}
}

func TokenHash(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}
