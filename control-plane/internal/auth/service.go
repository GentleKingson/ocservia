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
	ErrUnauthenticated       = errors.New("principal is not authenticated")
	ErrOIDCState             = errors.New("OIDC state is invalid")
	ErrBreakGlassDisabled    = errors.New("break-glass is disabled")
	ErrBreakGlassRotationDue = errors.New("break-glass credential rotation is required")
)

type Config struct {
	Issuer, ClientID, ClientSecret, RedirectURL string
	SessionKey                                  []byte
	SessionTTL                                  time.Duration
	BreakGlassEnabled                           bool
	BreakGlassTokenHash                         []byte
}

type Service struct {
	pool                *pgxpool.Pool
	issuer              string
	clientID            string
	clientSecret        string
	redirectURL         string
	aead                cipher.AEAD
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
	if pool == nil || len(cfg.SessionKey) != 32 || cfg.SessionTTL < time.Minute || cfg.SessionTTL > 24*time.Hour {
		return nil, errors.New("invalid authentication configuration")
	}
	redirect, err := url.Parse(cfg.RedirectURL)
	if err != nil || redirect.Scheme != "https" || redirect.Host == "" || redirect.Fragment != "" {
		return nil, errors.New("OIDC redirect URL must be an absolute HTTPS URL")
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
		pool: pool, issuer: strings.TrimSuffix(cfg.Issuer, "/"), clientID: cfg.ClientID, clientSecret: cfg.ClientSecret, redirectURL: cfg.RedirectURL, aead: aead, sessionTTL: cfg.SessionTTL,
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
	return s.createSession(ctx, s.issuer, identity.Subject, identity.Email, identity.Name, false)
}

func (s *Service) provider(ctx context.Context) (oauth2.Config, *oidc.IDTokenVerifier, error) {
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
	err = s.pool.QueryRow(ctx, `SELECT i.issuer,i.subject,s.break_glass,s.expires_at FROM auth_sessions s JOIN identities i ON i.id=s.identity_id WHERE s.id=$1 AND s.identity_id=$2 AND s.revoked_at IS NULL AND s.expires_at>now() AND i.disabled_at IS NULL`, sessionID, identityID).Scan(&principal.Issuer, &principal.Subject, &principal.BreakGlass, &principal.ExpiresAt)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	if envelope.ExpiresAt.Before(principal.ExpiresAt) {
		principal.ExpiresAt = envelope.ExpiresAt
	}
	return principal, nil
}

func (s *Service) Logout(ctx context.Context, principal Principal) error {
	_, err := s.pool.Exec(ctx, `UPDATE auth_sessions SET revoked_at=now() WHERE id=$1 AND identity_id=$2 AND revoked_at IS NULL`, principal.SessionID, principal.IdentityID)
	return err
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, Principal{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var used bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM break_glass_uses WHERE credential_fingerprint=$1 AND rotation_required=true)`, fingerprint[:]).Scan(&used); err != nil {
		return nil, Principal{}, err
	}
	if used {
		return nil, Principal{}, ErrBreakGlassRotationDue
	}
	now := s.now()
	identityID := uuid.Must(uuid.NewV7())
	if err := tx.QueryRow(ctx, `INSERT INTO identities(id,issuer,subject,display_name,created_at,updated_at) VALUES($1,'break-glass','offline','Break-glass',$2,$2) ON CONFLICT(issuer,subject) DO UPDATE SET updated_at=EXCLUDED.updated_at RETURNING id`, identityID, now).Scan(&identityID); err != nil {
		return nil, Principal{}, err
	}
	sessionID := uuid.Must(uuid.NewV7())
	expires := now.Add(15 * time.Minute)
	if _, err := tx.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,break_glass,created_at) VALUES($1,$2,$3,true,$4)`, sessionID, identityID, expires, now); err != nil {
		return nil, Principal{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO break_glass_uses(credential_fingerprint,identity_id,used_at,source_session_id,rotation_required) VALUES($1,$2,$3,$4,true)`, fingerprint[:], identityID, now, sessionID); err != nil {
		return nil, Principal{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO security_alerts(id,severity,kind,source_session_id,created_at) VALUES($1,'critical','break_glass.used',$2,$3)`, uuid.Must(uuid.NewV7()), sessionID, now); err != nil {
		return nil, Principal{}, err
	}
	// Break-glass is platform-scoped and must be visible in every workspace chain.
	rows, err := tx.Query(ctx, `SELECT id FROM workspaces ORDER BY id`)
	if err != nil {
		return nil, Principal{}, err
	}
	var workspaces []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, Principal{}, err
		}
		workspaces = append(workspaces, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, Principal{}, err
	}
	for _, workspaceID := range workspaces {
		if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "break_glass", ActorID: identityID.String(), SessionID: &sessionID, Action: "break_glass.use", ResourceType: "platform", RequestID: requestID, Result: "succeeded", Reason: "emergency offline access", At: now}); err != nil {
			return nil, Principal{}, fmt.Errorf("append break-glass audit: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, Principal{}, err
	}
	value, err := s.seal(sessionEnvelope{SessionID: sessionID.String(), IdentityID: identityID.String(), ExpiresAt: expires})
	if err != nil {
		return nil, Principal{}, err
	}
	return secureCookie(SessionCookieName, value, expires), Principal{IdentityID: identityID, SessionID: sessionID, Subject: "offline", Issuer: "break-glass", BreakGlass: true, ExpiresAt: expires}, nil
}

func (s *Service) createSession(ctx context.Context, issuer, subject, email, name string, breakGlass bool) (*http.Cookie, Principal, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, Principal{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	now := s.now()
	identityID := uuid.Must(uuid.NewV7())
	if err := tx.QueryRow(ctx, `INSERT INTO identities(id,issuer,subject,email,display_name,created_at,updated_at) VALUES($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,$6) ON CONFLICT(issuer,subject) DO UPDATE SET email=EXCLUDED.email,display_name=EXCLUDED.display_name,updated_at=EXCLUDED.updated_at RETURNING id`, identityID, issuer, subject, email, name, now).Scan(&identityID); err != nil {
		return nil, Principal{}, err
	}
	sessionID := uuid.Must(uuid.NewV7())
	expires := now.Add(s.sessionTTL)
	if _, err := tx.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,break_glass,created_at) VALUES($1,$2,$3,$4,$5)`, sessionID, identityID, expires, breakGlass, now); err != nil {
		return nil, Principal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, Principal{}, err
	}
	value, err := s.seal(sessionEnvelope{SessionID: sessionID.String(), IdentityID: identityID.String(), ExpiresAt: expires})
	if err != nil {
		return nil, Principal{}, err
	}
	return secureCookie(SessionCookieName, value, expires), Principal{IdentityID: identityID, SessionID: sessionID, Subject: subject, Issuer: issuer, BreakGlass: breakGlass, ExpiresAt: expires}, nil
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
