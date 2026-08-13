package privdattestation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidRequest = errors.New("privd attestation request is invalid")
	ErrCredential     = errors.New("privd attestation credential is invalid or consumed")
	ErrRotationLimit  = errors.New("privd attestation rotation overlap is full")
	ErrKeyNotFound    = errors.New("privd attestation key was not found")
)

const AttestationCapability = "privd_result_attestation_v1"

type KeyStateMetric struct {
	State string `json:"state"`
	Total int64  `json:"total"`
}

// KeyStateMetrics returns only the three protocol-defined states, keeping the
// metric label set bounded even when a node fleet grows.
func KeyStateMetrics(ctx context.Context, pool *pgxpool.Pool) ([]KeyStateMetric, error) {
	metrics := []KeyStateMetric{{State: "pending"}, {State: "active"}, {State: "revoked"}}
	if pool == nil {
		return metrics, nil
	}
	rows, err := pool.Query(ctx, `SELECT state,count(*) FROM node_privd_attestation_keys GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var total int64
		if err := rows.Scan(&state, &total); err != nil {
			return nil, err
		}
		for index := range metrics {
			if metrics[index].State == state {
				metrics[index].Total = total
			}
		}
	}
	return metrics, rows.Err()
}

type Service struct {
	pool            *pgxpool.Pool
	now             func() time.Time
	random          func([]byte) error
	rotationOverlap time.Duration
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{
		pool: pool, now: func() time.Time { return time.Now().UTC() },
		random:          func(value []byte) error { _, err := rand.Read(value); return err },
		rotationOverlap: 24 * time.Hour,
	}
}

type CredentialRequest struct {
	NodeID     uuid.UUID
	IdentityID uuid.UUID
	SessionID  uuid.UUID
	TTL        time.Duration
	RequestID  string
	Reason     string
}

type Credential struct {
	ID                      uuid.UUID `json:"id"`
	NodeID                  uuid.UUID `json:"node_id"`
	Value                   string    `json:"credential"`
	ControllerNonce         []byte    `json:"controller_nonce"`
	CredentialContextSHA256 []byte    `json:"credential_context_sha256"`
	ExpiresAt               time.Time `json:"expires_at"`
}

func (s *Service) CreateCredential(ctx context.Context, request CredentialRequest) (Credential, error) {
	if s.pool == nil || request.NodeID.Version() != 7 || request.IdentityID.Version() != 7 || request.SessionID.Version() != 7 || request.TTL < 5*time.Minute || request.TTL > time.Hour || request.RequestID == "" || strings.TrimSpace(request.Reason) == "" {
		return Credential{}, ErrInvalidRequest
	}
	now := s.now()
	expiresAt := now.Add(request.TTL)
	id, err := uuid.NewV7()
	if err != nil {
		return Credential{}, err
	}
	secret, nonce := make([]byte, 32), make([]byte, 32)
	if err := s.random(secret); err != nil {
		return Credential{}, fmt.Errorf("generate attestation credential: %w", err)
	}
	if err := s.random(nonce); err != nil {
		return Credential{}, fmt.Errorf("generate attestation nonce: %w", err)
	}
	secretDigest := sha256.Sum256(secret)
	contextDigest := credentialContext(id, request.NodeID, nonce, expiresAt)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Credential{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var workspaceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT workspace_id FROM nodes WHERE id=$1 AND status IN ('active','offline') FOR UPDATE`, request.NodeID).Scan(&workspaceID); err != nil {
		return Credential{}, err
	}
	var outstanding bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM privd_attestation_enrollment_credentials WHERE node_id=$1 AND consumed_at IS NULL AND expires_at>$2)`, request.NodeID, now).Scan(&outstanding); err != nil {
		return Credential{}, err
	}
	if outstanding {
		return Credential{}, ErrCredential
	}
	if _, err := tx.Exec(ctx, `INSERT INTO privd_attestation_enrollment_credentials(id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,created_by_identity_id,created_by_session_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, request.NodeID, secretDigest[:], nonce, contextDigest[:], expiresAt, request.IdentityID, request.SessionID, now); err != nil {
		return Credential{}, err
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{
		WorkspaceID: workspaceID, ActorType: "user", ActorID: request.IdentityID.String(), SessionID: &request.SessionID,
		Action: "privd.attestation.credential.create", ResourceType: "node", ResourceID: request.NodeID,
		NodeID: &request.NodeID, RequestID: request.RequestID, Result: "succeeded", Reason: request.Reason, At: now,
	}); err != nil {
		return Credential{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Credential{}, err
	}
	return Credential{
		ID: id, NodeID: request.NodeID, Value: base64.RawURLEncoding.EncodeToString(secret),
		ControllerNonce: nonce, CredentialContextSHA256: contextDigest[:], ExpiresAt: expiresAt,
	}, nil
}

type RegistrationRequest struct {
	NodeID       uuid.UUID
	Credential   string
	Registration *agentv1.PrivdAttestationRegistrationV1
	RequestID    string
}

func (s *Service) Register(ctx context.Context, request RegistrationRequest) (string, error) {
	if s.pool == nil || request.NodeID.Version() != 7 || request.Registration == nil || request.RequestID == "" || len(request.Credential) != 43 {
		return "", ErrInvalidRequest
	}
	secret, err := base64.RawURLEncoding.DecodeString(request.Credential)
	if err != nil || len(secret) != 32 {
		return "", ErrCredential
	}
	canonical, err := CanonicalRegistrationV1(request.Registration)
	if err != nil || len(request.Registration.GetSignature()) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(request.Registration.GetPublicKey()), canonical, request.Registration.GetSignature()) {
		return "", ErrInvalidRequest
	}
	if !bytes.Equal(request.Registration.GetNodeId(), request.NodeID[:]) {
		return "", ErrCredential
	}
	secretDigest := sha256.Sum256(secret)
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var credentialID, nodeID uuid.UUID
	var storedSecret, nonce, contextDigest []byte
	var expiresAt time.Time
	var consumedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,consumed_at FROM privd_attestation_enrollment_credentials WHERE secret_sha256=$1 FOR UPDATE`, secretDigest[:]).Scan(&credentialID, &nodeID, &storedSecret, &nonce, &contextDigest, &expiresAt, &consumedAt)
	if err != nil || nodeID != request.NodeID || consumedAt != nil || !expiresAt.After(now) || subtle.ConstantTimeCompare(storedSecret, secretDigest[:]) != 1 || subtle.ConstantTimeCompare(nonce, request.Registration.GetControllerNonce()) != 1 || subtle.ConstantTimeCompare(contextDigest, request.Registration.GetCredentialContextSha256()) != 1 {
		return "", ErrCredential
	}
	var workspaceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT workspace_id FROM nodes WHERE id=$1 AND status IN ('active','offline') FOR UPDATE`, request.NodeID).Scan(&workspaceID); err != nil {
		return "", ErrCredential
	}
	rows, err := tx.Query(ctx, `SELECT key_id FROM node_privd_attestation_keys WHERE node_id=$1 AND state='active' AND (valid_until IS NULL OR valid_until>$2) ORDER BY activated_at FOR UPDATE`, request.NodeID, now)
	if err != nil {
		return "", err
	}
	var active []string
	for rows.Next() {
		var keyID string
		if err := rows.Scan(&keyID); err != nil {
			rows.Close()
			return "", err
		}
		active = append(active, keyID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(active) >= 2 {
		return "", ErrRotationLimit
	}
	keyID := request.Registration.GetPrivdAttestationKeyId()
	predecessor := any(nil)
	if len(active) == 1 {
		predecessor = active[0]
	}
	if _, err := tx.Exec(ctx, `INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,predecessor_key_id,registration_credential_id) VALUES($1,$2,'ed25519',$3,'active',$4,$4,$4,$5,$6)`, request.NodeID, keyID, request.Registration.GetPublicKey(), now, predecessor, credentialID); err != nil {
		return "", err
	}
	if len(active) == 1 {
		if _, err := tx.Exec(ctx, `UPDATE node_privd_attestation_keys SET valid_until=$3,successor_key_id=$2 WHERE node_id=$1 AND key_id=$4 AND state='active'`, request.NodeID, keyID, now.Add(s.rotationOverlap), active[0]); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE privd_attestation_enrollment_credentials SET consumed_at=$2 WHERE id=$1 AND consumed_at IS NULL`, credentialID, now); err != nil {
		return "", err
	}
	// Consuming the independent root-only credential is the approval event for
	// this Controller-side capability. Session negotiation still requires the
	// upgraded Agent to advertise the same capability before transport can use it.
	if _, err := tx.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,$2,true) ON CONFLICT(node_id,capability) DO UPDATE SET approved=true`, request.NodeID, AttestationCapability); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE nodes SET authorization_revision=authorization_revision+1,version=version+1,updated_at=$2 WHERE id=$1`, request.NodeID, now); err != nil {
		return "", err
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{
		WorkspaceID: workspaceID, ActorType: "privd_provisioning", ActorID: keyID,
		Action: "privd.attestation.key.register", ResourceType: "node", ResourceID: request.NodeID,
		NodeID: &request.NodeID, RequestID: request.RequestID, Result: "succeeded", Reason: "root-authenticated one-time registration", At: now,
	}); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return keyID, nil
}

type RevokeRequest struct {
	NodeID, IdentityID, SessionID uuid.UUID
	KeyID, RequestID, Reason      string
}

func (s *Service) Revoke(ctx context.Context, request RevokeRequest) error {
	if s.pool == nil || request.NodeID.Version() != 7 || request.IdentityID.Version() != 7 || request.SessionID.Version() != 7 || !validKeyID(request.KeyID) || request.RequestID == "" || strings.TrimSpace(request.Reason) == "" {
		return ErrInvalidRequest
	}
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var workspaceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT workspace_id FROM nodes WHERE id=$1 FOR UPDATE`, request.NodeID).Scan(&workspaceID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE node_privd_attestation_keys SET state='revoked',revoked_at=$3,valid_until=LEAST(COALESCE(valid_until,$3),$3) WHERE node_id=$1 AND key_id=$2 AND state='active'`, request.NodeID, request.KeyID, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrKeyNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE nodes SET authorization_revision=authorization_revision+1,version=version+1,updated_at=$2 WHERE id=$1`, request.NodeID, now); err != nil {
		return err
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{
		WorkspaceID: workspaceID, ActorType: "user", ActorID: request.IdentityID.String(), SessionID: &request.SessionID,
		Action: "privd.attestation.key.revoke", ResourceType: "node", ResourceID: request.NodeID, NodeID: &request.NodeID,
		RequestID: request.RequestID, Result: "succeeded", Reason: request.Reason, At: now,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func credentialContext(id, nodeID uuid.UUID, nonce []byte, expiresAt time.Time) [32]byte {
	value := []byte("ocservia/privd-attestation-credential/v1\x00")
	value = append(value, id[:]...)
	value = append(value, nodeID[:]...)
	value = append(value, nonce...)
	value = fmt.Appendf(value, "%020d", expiresAt.Unix())
	return sha256.Sum256(value)
}
