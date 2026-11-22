package certificates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/google/uuid"
)

func (s *Service) Issue(ctx context.Context, request IssueRequest) (Certificate, error) {
	if request.CertificateID == uuid.Nil || request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil || strings.TrimSpace(request.Reason) == "" || request.RequestID == "" {
		return Certificate{}, ErrInvalid
	}
	if s.signer == nil {
		return Certificate{}, ErrSignerUnavailable
	}
	workspaceID, nodeID, csrDER, requestHash, err := s.prepareIssue(ctx, &request)
	if err != nil {
		return Certificate{}, err
	}
	approved, err := s.signingReceipt(ctx, request.CertificateID, nodeID, csrDER, requestHash)
	if err != nil {
		return Certificate{}, err
	}
	result, err := s.signer.Sign(ctx, SignRequest{CertificateID: request.CertificateID, CSRDER: append([]byte(nil), csrDER...)})
	if err != nil {
		_ = database.Within(context.WithoutCancel(ctx), s.backend, database.ReadCommitted, func(tx database.Tx) error {
			store, storeErr := certificatestore.FromTransaction(tx)
			if storeErr != nil {
				return storeErr
			}
			return store.SignerUnavailable(context.WithoutCancel(ctx), request.CertificateID, requestHash, s.now())
		})
		return Certificate{}, fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
	}
	leaf, err := validateSignedCertificate(csrDER, result.CertificateChainPEM, s.now())
	if err != nil {
		return Certificate{}, err
	}
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		now := s.now()
		current, err := store.LockReceipt(ctx, request.CertificateID)
		if err != nil || current.NodeID != nodeID || !current.Current || current.ReceiptKey != approved.ReceiptKey || !bytes.Equal(current.CSR, csrDER) || !bytes.Equal(current.ReceiptDigest, approved.ReceiptDigest) || !bytes.Equal(current.CSRDigest, approved.CSRDigest) || !bytes.Equal(current.SubjectDigest, approved.SubjectDigest) || !bytes.Equal(current.RequestHash, requestHash) {
			return ErrNotReady
		}
		active, err := store.KeyActive(ctx, nodeID, current.ReceiptKey, now.UTC())
		if err != nil || !active {
			return ErrNotReady
		}
		changed, err := store.CompleteIssue(ctx, certificatestore.Issued{ID: request.CertificateID, Chain: result.CertificateChainPEM, RequestHash: requestHash, Serial: leaf.SerialNumber.String(), NotBefore: leaf.NotBefore.UTC(), NotAfter: leaf.NotAfter.UTC(), At: now})
		if err != nil {
			return err
		}
		if !changed {
			return ErrNotReady
		}
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorIdentityID.String(), SessionID: &request.ActorSessionID, Action: "certificate.issue", ResourceType: "certificate", ResourceID: request.CertificateID, RequestID: request.RequestID, Result: "succeeded", Reason: request.Reason, AfterSummary: json.RawMessage(fmt.Sprintf(`{"node_id":%q,"serial_number":%q,"not_after":%q}`, nodeID, leaf.SerialNumber.String(), leaf.NotAfter.UTC().Format(time.RFC3339))), At: now})
	})
	if err != nil {
		return Certificate{}, err
	}
	return s.Get(ctx, request.CertificateID)
}

func (s *Service) prepareIssue(ctx context.Context, request *IssueRequest) (workspaceID, nodeID uuid.UUID, csrDER, requestHash []byte, err error) {
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		stored, err := store.LockIssue(ctx, request.CertificateID)
		if errors.Is(err, database.ErrNotFound) {
			return ErrNotReady
		}
		if err != nil {
			return err
		}
		workspaceID, nodeID, csrDER = stored.WorkspaceID, stored.NodeID, stored.CSR
		if err := auth.AuthorizeMutationTx(ctx, tx, request.ActorIdentityID, request.ActorSessionID, workspaceID); err != nil {
			return err
		}
		var dnsNames []string
		if err := json.Unmarshal(stored.DNSNames, &dnsNames); err != nil {
			return ErrNotReady
		}
		csrDigest := sha256.Sum256(csrDER)
		subjectDigest, subjectErr := privdattestation.RequestedSubjectDigest(&agentv1.CertificateCsr{CertificateId: request.CertificateID[:], CommonName: stored.CommonName, DnsNames: dnsNames, KeyBits: stored.KeyBits})
		if !stored.ReceiptCurrent || len(stored.ReceiptDigest) != sha256.Size || len(stored.ReceiptKey) == 0 || !bytes.Equal(stored.CSRDigest, csrDigest[:]) || subjectErr != nil || !bytes.Equal(stored.SubjectDigest, subjectDigest) {
			return ErrNotReady
		}
		at, err := database.TransactionTime(ctx, tx)
		if err != nil {
			return err
		}
		active, err := store.KeyActive(ctx, nodeID, stored.ReceiptKey, at)
		if err != nil || !active {
			return ErrNotReady
		}
		switch stored.State {
		case "csr_ready":
			requestHash = certificateIssueBinding(request.CertificateID, stored.Version, csrDER, stored.ReceiptDigest)
			now := s.now()
			if err := store.StartIssue(ctx, certificatestore.Signing{ID: request.CertificateID, ActorID: request.ActorIdentityID, RequestHash: requestHash, Version: stored.Version, At: now}); err != nil {
				return err
			}
			return audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorIdentityID.String(), SessionID: &request.ActorSessionID, Action: "certificate.issue", ResourceType: "certificate", ResourceID: request.CertificateID, NodeID: &nodeID, RequestID: request.RequestID, Result: "intent", Reason: request.Reason, AfterSummary: json.RawMessage(fmt.Sprintf(`{"csr_sha256":%q,"state":"signing"}`, hex.EncodeToString(sha256Bytes(csrDER)))), At: now})
		case "signing", "signer_unavailable":
			if stored.IssueVersion == nil || *stored.IssueVersion < 1 {
				return ErrNotReady
			}
			requestHash = certificateIssueBinding(request.CertificateID, *stored.IssueVersion, csrDER, stored.ReceiptDigest)
			if stored.ActorID == nil || *stored.ActorID != request.ActorIdentityID || !bytes.Equal(stored.RequestHash, requestHash) {
				return ErrNotReady
			}
			if stored.State == "signer_unavailable" {
				return store.ResumeIssue(ctx, request.CertificateID, s.now())
			}
			return nil
		default:
			return ErrNotReady
		}
	})
	if err != nil {
		return uuid.Nil, uuid.Nil, nil, nil, err
	}
	return
}

func certificateIssueBinding(certificateID uuid.UUID, version int64, csrDER, receiptDigest []byte) []byte {
	value := []byte("ocservia/certificate-issue-approval/v2\x00")
	value = append(value, certificateID[:]...)
	value = binary.BigEndian.AppendUint64(value, uint64(version))
	value = append(value, sha256Bytes(csrDER)...)
	value = append(value, receiptDigest...)
	digest := sha256.Sum256(value)
	return digest[:]
}

func (s *Service) Revoke(ctx context.Context, request RevokeRequest) (operations.Operation, bool, error) {
	if request.CertificateID == uuid.Nil || request.CertificateVersion < 1 || request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil || request.ExpectedVersion < 1 || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.Reason) == "" || request.RequestID == "" {
		return operations.Operation{}, false, ErrInvalid
	}
	if s.signer == nil {
		return operations.Operation{}, false, ErrSignerUnavailable
	}
	certificate, err := s.Get(ctx, request.CertificateID)
	if err != nil {
		return operations.Operation{}, false, err
	}
	workspaceID, nodeID, serialNumber, state, certificateVersion := certificate.WorkspaceID, certificate.NodeID, certificate.SerialNumber, certificate.State, certificate.Version
	existingRequest, err := s.operationExists(ctx, workspaceID, request.IdempotencyKey)
	if err != nil {
		return operations.Operation{}, false, err
	}
	if certificateVersion != request.CertificateVersion && !existingRequest {
		return operations.Operation{}, false, ErrNotReady
	}
	if state == "revoked" {
		exists, err := s.operationExists(ctx, workspaceID, request.IdempotencyKey)
		if err != nil {
			return operations.Operation{}, false, err
		}
		if !exists {
			return operations.Operation{}, false, ErrNotReady
		}
	}
	if state != "issued" && state != "expiring" && state != "expired" && state != "revoking" && state != "revocation_unknown" && state != "revoked" || serialNumber == "" {
		return operations.Operation{}, false, ErrNotReady
	}
	op, replay, err := s.operations.Create(ctx, operations.CreateRequest{
		NodeID:          nodeID,
		ExpectedVersion: request.ExpectedVersion,
		IdempotencyKey:  request.IdempotencyKey,
		ActorID:         request.ActorIdentityID.String(),
		ActorIdentityID: request.ActorIdentityID,
		ActorSessionID:  request.ActorSessionID,
		Action:          "certificate.revoke",
		Reason:          request.Reason,
		RequestID:       request.RequestID,
		Traceparent:     request.Traceparent,
		TTL:             15 * time.Minute,
		Payload: operations.CertificateRevokePayload{
			CertificateID:      request.CertificateID,
			CertificateVersion: uint64(request.CertificateVersion),
			RevocationReason:   request.Reason,
			HoldDispatch:       true,
		},
	})
	if err != nil {
		return operations.Operation{}, false, err
	}
	if replay && op.State == "expired" {
		return op, true, ErrNotReady
	}
	if replay && state == "revoked" {
		return op, true, nil
	}
	if err := s.revokeAtSigner(ctx, request, serialNumber); err != nil {
		return op, replay, err
	}
	operationID, parseErr := uuid.Parse(op.ID)
	if parseErr != nil {
		return op, replay, parseErr
	}
	if err := s.releaseOperation(ctx, operationID); err != nil {
		return op, replay, err
	}
	return op, replay, nil
}

func (s *Service) signingReceipt(ctx context.Context, certificateID, nodeID uuid.UUID, csrDER, requestHash []byte) (certificatestore.IssueReceipt, error) {
	var approved certificatestore.IssueReceipt
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		approved, err = store.SigningReceipt(ctx, certificateID)
		return err
	})
	csrDigest := sha256.Sum256(csrDER)
	if err != nil || approved.NodeID != nodeID || !approved.Current || !approved.KeyActive || !bytes.Equal(approved.RequestHash, requestHash) || !bytes.Equal(approved.CSRDigest, csrDigest[:]) {
		return approved, ErrNotReady
	}
	return approved, nil
}

func (s *Service) revokeAtSigner(ctx context.Context, request RevokeRequest, serialNumber string) error {
	if err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		return store.BeginRevocation(ctx, request.CertificateID, s.now())
	}); err != nil {
		return err
	}
	if err := s.signer.Revoke(ctx, RevokeSignerRequest{CertificateID: request.CertificateID, SerialNumber: serialNumber, Reason: request.Reason}); err != nil {
		_ = database.Within(context.WithoutCancel(ctx), s.backend, database.ReadCommitted, func(tx database.Tx) error {
			store, storeErr := certificatestore.FromTransaction(tx)
			if storeErr != nil {
				return storeErr
			}
			return store.RevocationUnknown(context.WithoutCancel(ctx), request.CertificateID, s.now())
		})
		return fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
	}
	return nil
}
