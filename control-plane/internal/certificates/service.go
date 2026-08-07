package certificates

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalid           = errors.New("invalid certificate request")
	ErrNotReady          = errors.New("certificate is not ready")
	ErrSignerUnavailable = errors.New("certificate signer is unavailable")
	ErrArtifactDenied    = errors.New("artifact access denied")
	ErrArtifactCapacity  = errors.New("artifact download capacity is exhausted")
)

type SignRequest struct {
	CertificateID uuid.UUID
	CSRDER        []byte
}

type SignResult struct {
	CertificateChainPEM []byte
}

// Signer is an external PKI/HSM boundary. Implementations must make
// CertificateID idempotent; the controller never receives a CA private key.
type Signer interface {
	Sign(context.Context, SignRequest) (SignResult, error)
	Revoke(context.Context, RevokeSignerRequest) error
}

type SecretSealer interface {
	Seal(context.Context, uuid.UUID, []byte) (sealed []byte, keyID string, err error)
}

type ArtifactFetcher interface {
	FetchArtifact(context.Context, uuid.UUID, uuid.UUID, int64) (io.ReadCloser, error)
}

type RevokeSignerRequest struct {
	CertificateID uuid.UUID
	SerialNumber  string
	Reason        string
}

type Service struct {
	pool       *pgxpool.Pool
	operations *operationstore.Service
	approvals  *approvals.Service
	signer     Signer
	sealer     SecretSealer
	artifacts  ArtifactFetcher
	now        func() time.Time
}

type CreateRequest struct {
	NodeID, ActorIdentityID, ActorSessionID uuid.UUID
	ExpectedVersion                         int64
	IdempotencyKey, CommonName              string
	DNSNames                                []string
	KeyBits                                 uint32
	ActorID, Reason, RequestID, Traceparent string
}

type Certificate struct {
	ID              uuid.UUID  `json:"id"`
	WorkspaceID     uuid.UUID  `json:"workspace_id"`
	NodeID          uuid.UUID  `json:"node_id"`
	OperationID     uuid.UUID  `json:"operation_id"`
	CommonName      string     `json:"common_name"`
	DNSNames        []string   `json:"dns_names"`
	KeyBits         uint32     `json:"key_bits"`
	State           string     `json:"state"`
	PublicKeySHA256 []byte     `json:"public_key_sha256,omitempty"`
	SerialNumber    string     `json:"serial_number,omitempty"`
	NotBefore       *time.Time `json:"not_before,omitempty"`
	NotAfter        *time.Time `json:"not_after,omitempty"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type IssueRequest struct {
	CertificateID, ApprovalID, ActorIdentityID, ActorSessionID uuid.UUID
	Reason, RequestID                                          string
}

type RevokeRequest struct {
	CertificateID, ActorIdentityID, ActorSessionID uuid.UUID
	ExpectedVersion                                int64
	IdempotencyKey, Reason, RequestID, Traceparent string
}

type P12Request struct {
	CertificateID, ActorIdentityID, ActorSessionID uuid.UUID
	ExpectedVersion                                int64
	IdempotencyKey, Reason, RequestID, Traceparent string
}

type ArtifactGrant struct {
	ArtifactID    uuid.UUID                `json:"artifact_id"`
	Operation     operationstore.Operation `json:"operation"`
	DownloadToken string                   `json:"download_token,omitempty"`
	Password      string                   `json:"password,omitempty"`
	ExpiresAt     time.Time                `json:"expires_at"`
}

type ArtifactDownload struct {
	Reader         io.ReadCloser
	ExpectedSHA256 []byte
	Size           int64
	NodeID         uuid.UUID
}

func New(pool *pgxpool.Pool, operations *operationstore.Service) *Service {
	return &Service{pool: pool, operations: operations, approvals: approvals.New(pool), now: func() time.Time { return time.Now().UTC() }}
}

func NewWithSigner(pool *pgxpool.Pool, operations *operationstore.Service, signer Signer) *Service {
	service := New(pool, operations)
	service.signer = signer
	return service
}

func NewWithDependencies(pool *pgxpool.Pool, operations *operationstore.Service, signer Signer, sealer SecretSealer, artifacts ArtifactFetcher) *Service {
	service := NewWithSigner(pool, operations, signer)
	service.sealer, service.artifacts = sealer, artifacts
	return service
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (Certificate, bool, error) {
	if request.NodeID == uuid.Nil || request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil || request.ExpectedVersion < 1 || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.CommonName) == "" || strings.TrimSpace(request.Reason) == "" {
		return Certificate{}, false, ErrInvalid
	}
	certificateID, err := uuid.NewV7()
	if err != nil {
		return Certificate{}, false, err
	}
	op, replay, err := s.operations.CreateSynthetic(ctx, operationstore.CreateRequest{
		NodeID: request.NodeID, ExpectedVersion: request.ExpectedVersion, IdempotencyKey: request.IdempotencyKey,
		Kind: operationstore.CertificateCSR, CertificateID: certificateID, CommonName: request.CommonName,
		DNSNames: request.DNSNames, KeyBits: request.KeyBits, ActorID: request.ActorID,
		ActorIdentityID: request.ActorIdentityID, ActorSessionID: request.ActorSessionID,
		Action: "certificate.issue", Reason: request.Reason, RequestID: request.RequestID,
		Traceparent: request.Traceparent, TTL: 15 * time.Minute,
	})
	if err != nil {
		return Certificate{}, false, err
	}
	if replay {
		operationID, parseErr := uuid.Parse(op.ID)
		if parseErr != nil {
			return Certificate{}, false, parseErr
		}
		return s.GetByOperation(ctx, operationID, true)
	}
	certificate, err := s.Get(ctx, certificateID)
	return certificate, false, err
}

func (s *Service) ApprovalBinding(ctx context.Context, id uuid.UUID) (workspaceID, nodeID uuid.UUID, requestHash []byte, summary json.RawMessage, err error) {
	var csrDER []byte
	var commonName string
	var dnsNames []string
	err = s.pool.QueryRow(ctx, `SELECT workspace_id,node_id,csr_der,common_name,dns_names FROM certificates WHERE id=$1 AND state IN ('csr_ready','signer_unavailable')`, id).Scan(&workspaceID, &nodeID, &csrDER, &commonName, &dnsNames)
	if err != nil {
		return
	}
	digest := sha256.Sum256(append(append([]byte(id.String()+"\x00"), csrDER...), []byte("\x00certificate.issue")...))
	requestHash = digest[:]
	summary, err = json.Marshal(map[string]any{"certificate_id": id, "node_id": nodeID, "common_name": commonName, "dns_names": dnsNames, "csr_sha256": hex.EncodeToString(sha256Bytes(csrDER))})
	return
}

func (s *Service) Issue(ctx context.Context, request IssueRequest) (Certificate, error) {
	if request.CertificateID == uuid.Nil || request.ApprovalID == uuid.Nil || request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil || strings.TrimSpace(request.Reason) == "" || request.RequestID == "" {
		return Certificate{}, ErrInvalid
	}
	if s.signer == nil {
		return Certificate{}, ErrSignerUnavailable
	}
	var workspaceID, nodeID uuid.UUID
	var csrDER []byte
	err := s.pool.QueryRow(ctx, `SELECT workspace_id,node_id,csr_der FROM certificates WHERE id=$1 AND state IN ('csr_ready','signer_unavailable')`, request.CertificateID).Scan(&workspaceID, &nodeID, &csrDER)
	if errors.Is(err, pgx.ErrNoRows) {
		return Certificate{}, ErrNotReady
	}
	if err != nil {
		return Certificate{}, err
	}
	requestHash := sha256.Sum256(append(append([]byte(request.CertificateID.String()+"\x00"), csrDER...), []byte("\x00certificate.issue")...))
	if err := s.approvals.ValidateApprovedBound(ctx, request.ApprovalID, workspaceID, request.ActorIdentityID, "certificate.issue", "certificate", request.CertificateID, requestHash[:]); err != nil {
		return Certificate{}, err
	}
	result, err := s.signer.Sign(ctx, SignRequest{CertificateID: request.CertificateID, CSRDER: append([]byte(nil), csrDER...)})
	if err != nil {
		_, _ = s.pool.Exec(context.WithoutCancel(ctx), `UPDATE certificates SET state='signer_unavailable',updated_at=$2 WHERE id=$1 AND state IN ('csr_ready','signer_unavailable')`, request.CertificateID, s.now())
		return Certificate{}, fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
	}
	leaf, err := validateSignedCertificate(csrDER, result.CertificateChainPEM, s.now())
	if err != nil {
		return Certificate{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Certificate{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := approvals.ConsumeBound(ctx, tx, request.ApprovalID, workspaceID, request.ActorIdentityID, "certificate.issue", "certificate", request.CertificateID, requestHash[:]); err != nil {
		return Certificate{}, err
	}
	now := s.now()
	resultTag, err := tx.Exec(ctx, `UPDATE certificates SET state='issued',certificate_chain_pem=$2,serial_number=$3,not_before=$4,not_after=$5,updated_at=$6 WHERE id=$1 AND state IN ('csr_ready','signer_unavailable')`, request.CertificateID, result.CertificateChainPEM, leaf.SerialNumber.String(), leaf.NotBefore.UTC(), leaf.NotAfter.UTC(), now)
	if err != nil {
		return Certificate{}, err
	}
	if resultTag.RowsAffected() != 1 {
		return Certificate{}, ErrNotReady
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorIdentityID.String(), SessionID: &request.ActorSessionID, Action: "certificate.issue", ResourceType: "certificate", ResourceID: request.CertificateID, ApprovalID: &request.ApprovalID, RequestID: request.RequestID, Result: "succeeded", Reason: request.Reason, AfterSummary: json.RawMessage(fmt.Sprintf(`{"node_id":%q,"serial_number":%q,"not_after":%q}`, nodeID, leaf.SerialNumber.String(), leaf.NotAfter.UTC().Format(time.RFC3339))), At: now}); err != nil {
		return Certificate{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Certificate{}, err
	}
	return s.Get(ctx, request.CertificateID)
}

func (s *Service) Revoke(ctx context.Context, request RevokeRequest) (operationstore.Operation, bool, error) {
	if request.CertificateID == uuid.Nil || request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil || request.ExpectedVersion < 1 || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.Reason) == "" || request.RequestID == "" {
		return operationstore.Operation{}, false, ErrInvalid
	}
	if s.signer == nil {
		return operationstore.Operation{}, false, ErrSignerUnavailable
	}
	var nodeID uuid.UUID
	var serialNumber, state string
	if err := s.pool.QueryRow(ctx, `SELECT node_id,COALESCE(serial_number,''),state FROM certificates WHERE id=$1`, request.CertificateID).Scan(&nodeID, &serialNumber, &state); err != nil {
		return operationstore.Operation{}, false, err
	}
	if state == "revoked" {
		return operationstore.Operation{}, false, ErrNotReady
	}
	if state != "issued" && state != "expiring" && state != "revoking" || serialNumber == "" {
		return operationstore.Operation{}, false, ErrNotReady
	}
	if err := s.signer.Revoke(ctx, RevokeSignerRequest{CertificateID: request.CertificateID, SerialNumber: serialNumber, Reason: request.Reason}); err != nil {
		return operationstore.Operation{}, false, fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
	}
	op, replay, err := s.operations.CreateSynthetic(ctx, operationstore.CreateRequest{NodeID: nodeID, ExpectedVersion: request.ExpectedVersion, IdempotencyKey: request.IdempotencyKey, Kind: operationstore.CertificateRevoke, CertificateID: request.CertificateID, RevocationReason: request.Reason, ActorID: request.ActorIdentityID.String(), ActorIdentityID: request.ActorIdentityID, ActorSessionID: request.ActorSessionID, Action: "certificate.revoke", Reason: request.Reason, RequestID: request.RequestID, Traceparent: request.Traceparent, TTL: 15 * time.Minute})
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	_, _ = s.pool.Exec(ctx, `UPDATE certificates SET state='revoking',updated_at=$2 WHERE id=$1 AND state IN ('issued','expiring')`, request.CertificateID, s.now())
	return op, replay, nil
}

func (s *Service) CreateP12(ctx context.Context, request P12Request) (ArtifactGrant, bool, error) {
	if request.CertificateID == uuid.Nil || request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil || request.ExpectedVersion < 1 || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.Reason) == "" || request.RequestID == "" {
		return ArtifactGrant{}, false, ErrInvalid
	}
	if s.sealer == nil {
		return ArtifactGrant{}, false, ErrSignerUnavailable
	}
	var workspaceID, nodeID uuid.UUID
	var chain []byte
	if err := s.pool.QueryRow(ctx, `SELECT workspace_id,node_id,certificate_chain_pem FROM certificates WHERE id=$1 AND state IN ('issued','expiring')`, request.CertificateID).Scan(&workspaceID, &nodeID, &chain); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ArtifactGrant{}, false, ErrNotReady
		}
		return ArtifactGrant{}, false, err
	}
	intent, _ := json.Marshal(map[string]any{"certificate_id": request.CertificateID, "node_id": nodeID, "expected_version": request.ExpectedVersion, "actor_identity_id": request.ActorIdentityID, "actor_session_id": request.ActorSessionID, "reason": request.Reason})
	intentHash := sha256.Sum256(intent)
	var existingArtifactID, existingOperationID uuid.UUID
	var existingExpires time.Time
	var sameIntent bool
	err := s.pool.QueryRow(ctx, `SELECT a.id,a.operation_id,a.expires_at,a.request_hash=$3 FROM operations o JOIN artifact_operations a ON a.operation_id=o.id WHERE o.workspace_id=$1 AND o.idempotency_key=$2`, workspaceID, request.IdempotencyKey, intentHash[:]).Scan(&existingArtifactID, &existingOperationID, &existingExpires, &sameIntent)
	if err == nil {
		if !sameIntent {
			return ArtifactGrant{}, false, operationstore.ErrIdempotencyConflict
		}
		op, getErr := s.operations.Get(ctx, existingOperationID)
		if getErr != nil {
			return ArtifactGrant{}, false, getErr
		}
		return ArtifactGrant{ArtifactID: existingArtifactID, Operation: op, ExpiresAt: existingExpires}, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ArtifactGrant{}, false, err
	}
	passwordBytes, tokenBytes := make([]byte, 24), make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		return ArtifactGrant{}, false, err
	}
	if _, err := rand.Read(tokenBytes); err != nil {
		return ArtifactGrant{}, false, err
	}
	password := base64.RawURLEncoding.EncodeToString(passwordBytes)
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	for index := range passwordBytes {
		passwordBytes[index] = 0
	}
	sealed, keyID, err := s.sealer.Seal(ctx, nodeID, []byte(password))
	if err != nil {
		return ArtifactGrant{}, false, fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
	}
	artifactID := uuid.Must(uuid.NewV7())
	expiresAt := s.now().Add(10 * time.Minute)
	tokenHash := sha256.Sum256([]byte(token))
	op, replay, err := s.operations.CreateSynthetic(ctx, operationstore.CreateRequest{NodeID: nodeID, ExpectedVersion: request.ExpectedVersion, IdempotencyKey: request.IdempotencyKey, Kind: operationstore.CertificateP12, CertificateID: request.CertificateID, CertificateChain: chain, SealedPassword: sealed, SecretKeyID: keyID, ArtifactID: artifactID, ArtifactMetadata: &operationstore.ArtifactMetadata{TokenSHA256: tokenHash[:], RequestHash: intentHash[:], ExpiresAt: expiresAt}, ActorID: request.ActorIdentityID.String(), ActorIdentityID: request.ActorIdentityID, ActorSessionID: request.ActorSessionID, Action: "certificate.p12.create", Reason: request.Reason, RequestID: request.RequestID, Traceparent: request.Traceparent, TTL: 15 * time.Minute})
	if err != nil {
		if errors.Is(err, operationstore.ErrIdempotencyConflict) {
			var id, operationID uuid.UUID
			var expiration time.Time
			var same bool
			lookupErr := s.pool.QueryRow(ctx, `SELECT a.id,a.operation_id,a.expires_at,a.request_hash=$3 FROM operations o JOIN artifact_operations a ON a.operation_id=o.id WHERE o.workspace_id=$1 AND o.idempotency_key=$2`, workspaceID, request.IdempotencyKey, intentHash[:]).Scan(&id, &operationID, &expiration, &same)
			if lookupErr == nil && same {
				existingOperation, getErr := s.operations.Get(ctx, operationID)
				if getErr != nil {
					return ArtifactGrant{}, false, getErr
				}
				return ArtifactGrant{ArtifactID: id, Operation: existingOperation, ExpiresAt: expiration}, true, nil
			}
		}
		return ArtifactGrant{}, false, err
	}
	if replay {
		var existingID uuid.UUID
		var existingExpiry time.Time
		if err := s.pool.QueryRow(ctx, `SELECT id,expires_at FROM artifact_operations WHERE operation_id=$1`, op.ID).Scan(&existingID, &existingExpiry); err != nil {
			return ArtifactGrant{}, false, err
		}
		return ArtifactGrant{ArtifactID: existingID, Operation: op, ExpiresAt: existingExpiry}, true, nil
	}
	return ArtifactGrant{ArtifactID: artifactID, Operation: op, DownloadToken: token, Password: password, ExpiresAt: expiresAt}, false, nil
}

func (s *Service) ArtifactResource(ctx context.Context, id uuid.UUID) (workspaceID, nodeID uuid.UUID, err error) {
	err = s.pool.QueryRow(ctx, `SELECT workspace_id,node_id FROM artifact_operations WHERE id=$1`, id).Scan(&workspaceID, &nodeID)
	return
}

func (s *Service) OpenArtifact(ctx context.Context, id uuid.UUID, token string) (ArtifactDownload, error) {
	if id == uuid.Nil || len(token) != 43 || s.artifacts == nil {
		return ArtifactDownload{}, ErrArtifactDenied
	}
	tokenHash := sha256.Sum256([]byte(token))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ArtifactDownload{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(6820260817)`); err != nil {
		return ArtifactDownload{}, err
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM artifact_operations WHERE state='leased' AND lease_until>now()`).Scan(&active); err != nil {
		return ArtifactDownload{}, err
	}
	if active >= 4 {
		return ArtifactDownload{}, ErrArtifactCapacity
	}
	var nodeID uuid.UUID
	var expected []byte
	var size int64
	err = tx.QueryRow(ctx, `SELECT node_id,content_sha256,content_size FROM artifact_operations WHERE id=$1 AND token_sha256=$2 AND expires_at>now() AND (state='ready' OR (state='leased' AND lease_until<now())) FOR UPDATE`, id, tokenHash[:]).Scan(&nodeID, &expected, &size)
	if err != nil {
		return ArtifactDownload{}, ErrArtifactDenied
	}
	if _, err := tx.Exec(ctx, `UPDATE artifact_operations SET state='leased',lease_until=now()+interval '1 minute',updated_at=now() WHERE id=$1`, id); err != nil {
		return ArtifactDownload{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ArtifactDownload{}, err
	}
	reader, err := s.artifacts.FetchArtifact(ctx, nodeID, id, min(size, 64<<20))
	if err != nil {
		_ = s.AbortArtifact(context.WithoutCancel(ctx), id)
		return ArtifactDownload{}, err
	}
	return ArtifactDownload{Reader: reader, ExpectedSHA256: expected, Size: size, NodeID: nodeID}, nil
}

func (s *Service) CompleteArtifact(ctx context.Context, id uuid.UUID, digest []byte, size int64, actorID, sessionID uuid.UUID, requestID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var workspaceID, nodeID, certificateID uuid.UUID
	err = tx.QueryRow(ctx, `UPDATE artifact_operations SET state='consumed',consumed_at=now(),lease_until=NULL,updated_at=now() WHERE id=$1 AND state='leased' AND content_sha256=$2 AND content_size=$3 AND expires_at>now() RETURNING workspace_id,node_id,certificate_id`, id, digest, size).Scan(&workspaceID, &nodeID, &certificateID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrArtifactDenied
	}
	if err != nil {
		return err
	}
	now := s.now()
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: actorID.String(), SessionID: &sessionID, Action: "certificate.p12.download", ResourceType: "artifact", ResourceID: id, NodeID: &nodeID, RequestID: requestID, Result: "succeeded", AfterSummary: json.RawMessage(fmt.Sprintf(`{"certificate_id":%q,"sha256":%q,"size":%d}`, certificateID, hex.EncodeToString(digest), size)), At: now}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) AbortArtifact(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE artifact_operations SET state=CASE WHEN expires_at>now() THEN 'ready' ELSE 'expired' END,lease_until=NULL,updated_at=now() WHERE id=$1 AND state='leased'`, id)
	return err
}

func (s *Service) Maintain(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(ctx, `WITH due AS (
		SELECT id FROM certificates WHERE state='issued' AND not_after<=now()+interval '30 days'
		ORDER BY not_after,id LIMIT 100 FOR UPDATE SKIP LOCKED
	) UPDATE certificates c SET state='expiring',updated_at=now() FROM due WHERE c.id=due.id RETURNING c.id,c.workspace_id,c.node_id,c.not_after`)
	if err != nil {
		return err
	}
	type expiryAlert struct {
		id          uuid.UUID
		workspaceID uuid.UUID
		nodeID      uuid.UUID
		notAfter    time.Time
	}
	alerts := make([]expiryAlert, 0, 100)
	for rows.Next() {
		var alert expiryAlert
		if err := rows.Scan(&alert.id, &alert.workspaceID, &alert.nodeID, &alert.notAfter); err != nil {
			rows.Close()
			return err
		}
		alerts = append(alerts, alert)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, alert := range alerts {
		if _, err := tx.Exec(ctx, `INSERT INTO security_alerts(id,workspace_id,severity,kind,node_id,resource_type,resource_id,created_at) VALUES($1,$2,'high','certificate.expiring',$3,'certificate',$4,$5)`, uuid.Must(uuid.NewV7()), alert.workspaceID, alert.nodeID, alert.id, s.now()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE artifact_operations SET state='expired',lease_until=NULL,updated_at=now() WHERE state IN ('pending','ready','leased') AND expires_at<=now()`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Certificate, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,public_key_sha256,COALESCE(serial_number,''),not_before,not_after,revoked_at,created_at,updated_at FROM certificates WHERE id=$1`, id))
}

func (s *Service) ListNode(ctx context.Context, nodeID uuid.UUID) ([]Certificate, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,public_key_sha256,COALESCE(serial_number,''),not_before,not_after,revoked_at,created_at,updated_at FROM certificates WHERE node_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]Certificate, 0)
	for rows.Next() {
		value, scanErr := scan(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Service) GetByOperation(ctx context.Context, operationID uuid.UUID, replay bool) (Certificate, bool, error) {
	certificate, err := scan(s.pool.QueryRow(ctx, `SELECT id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,public_key_sha256,COALESCE(serial_number,''),not_before,not_after,revoked_at,created_at,updated_at FROM certificates WHERE operation_id=$1`, operationID))
	return certificate, replay, err
}

func (s *Service) Resource(ctx context.Context, id uuid.UUID) (workspaceID, nodeID uuid.UUID, err error) {
	err = s.pool.QueryRow(ctx, `SELECT workspace_id,node_id FROM certificates WHERE id=$1`, id).Scan(&workspaceID, &nodeID)
	return
}

func scan(row pgx.Row) (Certificate, error) {
	var certificate Certificate
	var keyBits int32
	err := row.Scan(&certificate.ID, &certificate.WorkspaceID, &certificate.NodeID, &certificate.OperationID, &certificate.CommonName, &certificate.DNSNames, &keyBits, &certificate.State, &certificate.PublicKeySHA256, &certificate.SerialNumber, &certificate.NotBefore, &certificate.NotAfter, &certificate.RevokedAt, &certificate.CreatedAt, &certificate.UpdatedAt)
	if err != nil {
		return Certificate{}, err
	}
	certificate.KeyBits = uint32(keyBits)
	return certificate, nil
}

func validateSignedCertificate(csrDER, chainPEM []byte, now time.Time) (*x509.Certificate, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil || csr.CheckSignature() != nil {
		return nil, ErrInvalid
	}
	block, _ := pem.Decode(chainPEM)
	if block == nil || block.Type != "CERTIFICATE" || len(chainPEM) > 256*1024 {
		return nil, ErrInvalid
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil || leaf.IsCA || now.Before(leaf.NotBefore.Add(-5*time.Minute)) || !now.Before(leaf.NotAfter) || !bytes.Equal(csr.RawSubject, leaf.RawSubject) {
		return nil, ErrInvalid
	}
	csrNames, leafNames := slices.Clone(csr.DNSNames), slices.Clone(leaf.DNSNames)
	sort.Strings(csrNames)
	sort.Strings(leafNames)
	if !slices.Equal(csrNames, leafNames) {
		return nil, ErrInvalid
	}
	csrKey, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return nil, ErrInvalid
	}
	leafKey, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil || !strings.EqualFold(hex.EncodeToString(csrKey), hex.EncodeToString(leafKey)) {
		return nil, ErrInvalid
	}
	return leaf, nil
}

func sha256Bytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}
