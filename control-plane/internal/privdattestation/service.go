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
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation/attestationstore"
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
	if pool == nil {
		return KeyStateMetricsBackend(ctx, nil)
	}
	return KeyStateMetricsBackend(ctx, postgres.WrapPool(pool))
}

func KeyStateMetricsBackend(ctx context.Context, backend database.Backend) ([]KeyStateMetric, error) {
	metrics := []KeyStateMetric{{State: "pending"}, {State: "active"}, {State: "revoked"}}
	if backend == nil {
		return metrics, nil
	}
	err := database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := attestationstore.From(tx)
		if err != nil {
			return err
		}
		rows, err := store.KeyStateCounts(ctx)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var state string
			var total int64
			if err := rows.Scan(&state, &total); err != nil {
				return err
			}
			for index := range metrics {
				if metrics[index].State == state {
					metrics[index].Total = total
				}
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return metrics, nil
}

type Service struct {
	backend         database.Backend
	now             func() time.Time
	random          func([]byte) error
	rotationOverlap time.Duration
}

func New(pool *pgxpool.Pool) *Service {
	if pool == nil {
		return NewBackend(nil)
	}
	return NewBackend(postgres.WrapPool(pool))
}

// NewBackend runs the same enrollment, registration and revocation workflows
// on backend-owned storage. Controller engine eligibility is enforced by
// application startup, not by this constructor.
func NewBackend(backend database.Backend) *Service {
	return &Service{
		backend: backend, now: func() time.Time { return time.Now().UTC() },
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
	if s.backend == nil || request.NodeID.Version() != 7 || request.IdentityID.Version() != 7 || request.SessionID.Version() != 7 || request.TTL < 5*time.Minute || request.TTL > time.Hour || request.RequestID == "" || strings.TrimSpace(request.Reason) == "" {
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
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := attestationstore.From(tx)
		if err != nil {
			return err
		}
		workspaceID, err := store.LockActiveNode(ctx, request.NodeID)
		if err != nil {
			return err
		}
		outstanding, err := store.OutstandingCredential(ctx, request.NodeID, now)
		if err != nil {
			return err
		}
		if outstanding {
			return ErrCredential
		}
		if err := store.InsertCredential(ctx, attestationstore.Credential{
			ID: id, NodeID: request.NodeID, SecretSHA256: secretDigest[:], ControllerNonce: nonce,
			ContextSHA256: contextDigest[:], ExpiresAt: expiresAt, CreatedByIdentityID: request.IdentityID,
			CreatedBySessionID: request.SessionID, CreatedAt: now,
		}); err != nil {
			return err
		}
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{
			WorkspaceID: workspaceID, ActorType: "user", ActorID: request.IdentityID.String(), SessionID: &request.SessionID,
			Action: "privd.attestation.credential.create", ResourceType: "node", ResourceID: request.NodeID,
			NodeID: &request.NodeID, RequestID: request.RequestID, Result: "succeeded", Reason: request.Reason, At: now,
		})
	})
	if err != nil {
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
	if s.backend == nil || request.NodeID.Version() != 7 || request.Registration == nil || request.RequestID == "" || len(request.Credential) != 43 {
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
	keyID := ""
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := attestationstore.From(tx)
		if err != nil {
			return err
		}
		credential, err := store.LockCredential(ctx, secretDigest[:])
		if err != nil {
			return ErrCredential
		}
		if credential.NodeID != request.NodeID || credential.ConsumedAt != nil || !credential.ExpiresAt.After(now) ||
			subtle.ConstantTimeCompare(credential.SecretSHA256, secretDigest[:]) != 1 ||
			subtle.ConstantTimeCompare(credential.ControllerNonce, request.Registration.GetControllerNonce()) != 1 ||
			subtle.ConstantTimeCompare(credential.ContextSHA256, request.Registration.GetCredentialContextSha256()) != 1 {
			return ErrCredential
		}
		workspaceID, err := store.LockActiveNode(ctx, request.NodeID)
		if err != nil {
			return ErrCredential
		}
		active, err := store.ActiveKeys(ctx, request.NodeID, now)
		if err != nil {
			return err
		}
		if len(active) >= 2 {
			return ErrRotationLimit
		}
		keyID = request.Registration.GetPrivdAttestationKeyId()
		predecessor := (*string)(nil)
		if len(active) == 1 {
			value := active[0]
			predecessor = &value
		}
		if err := store.InsertKey(ctx, attestationstore.Key{
			NodeID: request.NodeID, KeyID: keyID, PublicKey: request.Registration.GetPublicKey(),
			CreatedAt: now, ApprovedAt: now, ActivatedAt: now, ValidUntil: nil,
			PredecessorKeyID: predecessor, RegistrationCredentialID: credential.ID,
		}); err != nil {
			return err
		}
		if len(active) == 1 {
			if err := store.RotatePredecessor(ctx, request.NodeID, active[0], keyID, now.Add(s.rotationOverlap)); err != nil {
				return err
			}
		}
		if err := store.ConsumeCredential(ctx, credential.ID, now); err != nil {
			return err
		}
		// Consuming the independent root-only credential is the approval event for
		// this Controller-side capability. Session negotiation still requires the
		// upgraded Agent to advertise the same capability before transport can use it.
		if err := store.ApproveCapability(ctx, request.NodeID, AttestationCapability); err != nil {
			return err
		}
		if err := store.BumpNodeRevision(ctx, request.NodeID, now); err != nil {
			return err
		}
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{
			WorkspaceID: workspaceID, ActorType: "privd_provisioning", ActorID: keyID,
			Action: "privd.attestation.key.register", ResourceType: "node", ResourceID: request.NodeID,
			NodeID: &request.NodeID, RequestID: request.RequestID, Result: "succeeded", Reason: "root-authenticated one-time registration", At: now,
		})
	})
	if err != nil {
		return "", err
	}
	return keyID, nil
}

type RevokeRequest struct {
	NodeID, IdentityID, SessionID uuid.UUID
	KeyID, RequestID, Reason      string
}

func (s *Service) Revoke(ctx context.Context, request RevokeRequest) error {
	if s.backend == nil || request.NodeID.Version() != 7 || request.IdentityID.Version() != 7 || request.SessionID.Version() != 7 || !validKeyID(request.KeyID) || request.RequestID == "" || strings.TrimSpace(request.Reason) == "" {
		return ErrInvalidRequest
	}
	now := s.now()
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := attestationstore.From(tx)
		if err != nil {
			return err
		}
		workspaceID, err := store.LockNode(ctx, request.NodeID)
		if err != nil {
			return err
		}
		revoked, err := store.RevokeKey(ctx, request.NodeID, request.KeyID, now)
		if err != nil {
			return err
		}
		if !revoked {
			return ErrKeyNotFound
		}
		if err := store.BumpNodeRevision(ctx, request.NodeID, now); err != nil {
			return err
		}
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{
			WorkspaceID: workspaceID, ActorType: "user", ActorID: request.IdentityID.String(), SessionID: &request.SessionID,
			Action: "privd.attestation.key.revoke", ResourceType: "node", ResourceID: request.NodeID,
			NodeID: &request.NodeID, RequestID: request.RequestID, Result: "succeeded", Reason: request.Reason, At: now,
		})
	})
}

func credentialContext(id, nodeID uuid.UUID, nonce []byte, expiresAt time.Time) [32]byte {
	value := []byte("ocservia/privd-attestation-credential/v1\x00")
	value = append(value, id[:]...)
	value = append(value, nodeID[:]...)
	value = append(value, nonce...)
	value = fmt.Appendf(value, "%020d", expiresAt.Unix())
	return sha256.Sum256(value)
}
