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
	"net/url"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/identityprofile"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
)

const (
	SessionCookieName = "__Host-ocservia_session"
	LoginCookieName   = "__Host-ocservia_oidc"
)

var (
	ErrUnauthenticated        = errors.New("principal is not authenticated")
	ErrOIDCState              = errors.New("OIDC state is invalid")
	ErrOIDCSessionUnavailable = errors.New("OIDC session creation is unavailable")
	ErrOIDCDisabled           = errors.New("OIDC is disabled")
	ErrLocalDisabled          = errors.New("local authentication is disabled")
	ErrBreakGlassDisabled     = errors.New("break-glass is disabled")
	ErrBreakGlassRotationDue  = errors.New("break-glass credential rotation is required")
)

type Config struct {
	LocalEnabled                                bool
	Issuer, ClientID, ClientSecret, RedirectURL string
	SessionKey                                  []byte
	SessionTTL                                  time.Duration
	BreakGlassEnabled                           bool
	BreakGlassTokenHash                         []byte
}

type Service struct {
	pool                *pgxpool.Pool
	backend             database.Backend
	localEnabled        bool
	oidcEnabled         bool
	issuer              string
	clientID            string
	clientSecret        string
	redirectURL         string
	aead                cipher.AEAD
	accountLogKey       []byte
	sessionTTL          time.Duration
	breakGlassEnabled   bool
	breakGlassTokenHash []byte
	now                 func() time.Time
	random              io.Reader
	discover            func(context.Context) (oauth2.Config, *oidc.IDTokenVerifier, error)
}

type Principal struct {
	IdentityID uuid.UUID
	SessionID  uuid.UUID
	Subject    string
	Issuer     string
	BreakGlass bool
	ExpiresAt  time.Time
}

type loginState struct {
	State, Nonce, Verifier string
	ExpiresAt              time.Time
}

type sessionEnvelope struct {
	SessionID, IdentityID string
	ExpiresAt             time.Time
}

type claims struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Nonce   string `json:"nonce"`
}

func New(_ context.Context, pool *pgxpool.Pool, cfg Config) (*Service, error) {
	if pool == nil {
		return nil, errors.New("invalid authentication configuration")
	}
	s, err := NewBackend(postgres.WrapPool(pool), cfg)
	if err == nil {
		s.pool = pool
	}
	return s, err
}

// NewBackend uses the same session and credential workflows with backend-owned
// storage. Controller engine eligibility is enforced by application startup.
func NewBackend(backend database.Backend, cfg Config) (*Service, error) {
	if backend == nil || len(cfg.SessionKey) != 32 || cfg.SessionTTL < time.Minute || cfg.SessionTTL > 24*time.Hour {
		return nil, errors.New("invalid authentication configuration")
	}
	oidcEnabled := cfg.Issuer != "" || cfg.ClientID != "" || cfg.ClientSecret != "" || cfg.RedirectURL != ""
	if oidcEnabled {
		issuer, err := url.Parse(cfg.Issuer)
		if err != nil || (issuer.Scheme != "https" && issuer.Scheme != "http") || issuer.Host == "" || cfg.ClientID == "" {
			return nil, errors.New("OIDC requires an HTTP(S) issuer and client ID")
		}
		redirect, err := url.Parse(cfg.RedirectURL)
		if err != nil || redirect.Scheme != "https" || redirect.Host == "" || redirect.Fragment != "" {
			return nil, errors.New("OIDC redirect URL must be an absolute HTTPS URL")
		}
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
		localEnabled: cfg.LocalEnabled, oidcEnabled: oidcEnabled,
		accountLogKey: deriveAccountLogKey(cfg.SessionKey),
		backend:       backend, issuer: cfg.Issuer, clientID: cfg.ClientID, clientSecret: cfg.ClientSecret, redirectURL: cfg.RedirectURL, aead: aead, sessionTTL: cfg.SessionTTL,
		breakGlassEnabled: cfg.BreakGlassEnabled, breakGlassTokenHash: cfg.BreakGlassTokenHash,
		now: func() time.Time { return time.Now().UTC() }, random: rand.Reader,
	}, nil
}

func (s *Service) BeginLogin(ctx context.Context) (string, *http.Cookie, error) {
	oauth, _, err := s.provider(ctx)
	if err != nil {
		return "", nil, err
	}
	state, err := randomURL(s.random, 32)
	if err != nil {
		return "", nil, err
	}
	nonce, err := randomURL(s.random, 32)
	if err != nil {
		return "", nil, err
	}
	verifier := oauth2.GenerateVerifier()
	expires := s.now().Add(5 * time.Minute)
	value, err := s.seal(loginState{State: state, Nonce: nonce, Verifier: verifier, ExpiresAt: expires})
	if err != nil {
		return "", nil, err
	}
	cookie := secureCookie(LoginCookieName, value, expires)
	return oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), cookie, nil
}

func (s *Service) CompleteLogin(ctx context.Context, state, code string, cookie *http.Cookie) (*http.Cookie, Principal, error) {
	var attempt loginState
	if cookie == nil || s.open(cookie.Value, &attempt) != nil || s.now().After(attempt.ExpiresAt) ||
		subtle.ConstantTimeCompare([]byte(state), []byte(attempt.State)) != 1 || strings.TrimSpace(code) == "" {
		return nil, Principal{}, ErrOIDCState
	}
	oauth, verifier, err := s.provider(ctx)
	if err != nil {
		return nil, Principal{}, err
	}
	token, err := oauth.Exchange(ctx, code, oauth2.VerifierOption(attempt.Verifier))
	if err != nil {
		return nil, Principal{}, fmt.Errorf("exchange OIDC code: %w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, Principal{}, errors.New("OIDC response omitted id_token")
	}
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, Principal{}, fmt.Errorf("verify OIDC ID token: %w", err)
	}
	var identity claims
	if err := idToken.Claims(&identity); err != nil {
		return nil, Principal{}, fmt.Errorf("decode OIDC claims: %w", err)
	}
	if identity.Subject == "" || subtle.ConstantTimeCompare([]byte(identity.Nonce), []byte(attempt.Nonce)) != 1 {
		return nil, Principal{}, errors.New("OIDC nonce or subject is invalid")
	}
	session, principal, err := s.createSession(ctx, s.issuer, identity.Subject, identity.Email, identity.Name, false, nil)
	// Disabled identities remain authentication denials, not infrastructure failures.
	if err != nil && !errors.Is(err, ErrUnauthenticated) {
		return nil, Principal{}, fmt.Errorf("%w: %w", ErrOIDCSessionUnavailable, err)
	}
	return session, principal, err
}

func (s *Service) LocalEnabled() bool { return s.localEnabled }

func (s *Service) OIDCEnabled() bool { return s.oidcEnabled }

func (s *Service) provider(ctx context.Context) (oauth2.Config, *oidc.IDTokenVerifier, error) {
	if !s.oidcEnabled {
		return oauth2.Config{}, nil, ErrOIDCDisabled
	}
	if s.discover != nil {
		return s.discover(ctx)
	}
	provider, err := oidc.NewProvider(ctx, s.issuer)
	if err != nil {
		return oauth2.Config{}, nil, fmt.Errorf("discover OIDC provider: %w", err)
	}
	config := oauth2.Config{ClientID: s.clientID, ClientSecret: s.clientSecret, Endpoint: provider.Endpoint(), RedirectURL: s.redirectURL, Scopes: []string{oidc.ScopeOpenID, "profile", "email"}}
	return config, provider.Verifier(&oidc.Config{ClientID: s.clientID}), nil
}

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
		if !session.ExpiresAt.Valid {
			return ErrUnauthenticated
		}
		if session.ExpiresAt.Micros != value.PositiveInfinity {
			expires, err := session.ExpiresAt.Time()
			if err != nil {
				return err
			}
			if expires.Before(principal.ExpiresAt) {
				principal.ExpiresAt = expires
			}
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
		// Break-glass is platform-scoped and must be visible in every workspace chain.
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

func (s *Service) createSession(ctx context.Context, issuer, subject, email, name string, breakGlass bool, local *localCredential) (*http.Cookie, Principal, error) {
	identityID := uuid.Must(uuid.NewV7())
	sessionID := uuid.Must(uuid.NewV7())
	var expires time.Time
	err := s.withAuthentication(ctx, func(tx database.Tx, store authstore.Store) error {
		now := s.now()
		expires = now.Add(s.sessionTTL)
		var err error
		if local != nil {
			// Recheck the verified credential under lock; never upsert a local identity.
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
		} else {
			// The conflict row stays locked through session insertion and commit.
			identityID, err = identityprofile.Upsert(ctx, tx, identityID, issuer, subject, email, name, now)
			if errors.Is(err, database.ErrNotFound) {
				return ErrUnauthenticated
			}
			if err != nil {
				return err
			}
		}
		return store.InsertSession(ctx, sessionID, identityID, expires, breakGlass, now)
	})
	if err != nil {
		return nil, Principal{}, err
	}
	value, err := s.seal(sessionEnvelope{SessionID: sessionID.String(), IdentityID: identityID.String(), ExpiresAt: expires})
	if err != nil {
		return nil, Principal{}, err
	}
	return secureCookie(SessionCookieName, value, expires), Principal{IdentityID: identityID, SessionID: sessionID, Subject: subject, Issuer: issuer, BreakGlass: breakGlass, ExpiresAt: expires}, nil
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

func randomURL(reader io.Reader, size int) (string, error) {
	data := make([]byte, size)
	if _, err := io.ReadFull(reader, data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func TokenHash(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}
