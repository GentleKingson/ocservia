package certificates

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func (s *Service) Issue(ctx context.Context, request IssueRequest) (Certificate, error) {
	if request.CertificateID == uuid.Nil || request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil || strings.TrimSpace(request.Reason) == "" || request.RequestID == "" {
		return Certificate{}, ErrInvalid
	}
	if s.signer == nil {
		return Certificate{}, ErrSignerUnavailable
	}
	workspaceID, nodeID, csrDER, issueVersion, err := s.prepareIssue(ctx, &request)
	if err != nil {
		return Certificate{}, err
	}
	approved, err := s.signingReceipt(ctx, request.CertificateID, nodeID, csrDER, issueVersion)
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
			return store.SignerUnavailable(context.WithoutCancel(ctx), request.CertificateID, issueVersion, s.now())
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
		if err != nil || current.NodeID != nodeID || !current.Current || current.ReceiptKey != approved.ReceiptKey || !bytes.Equal(current.CSR, csrDER) || !bytes.Equal(current.ReceiptDigest, approved.ReceiptDigest) || !bytes.Equal(current.CSRDigest, approved.CSRDigest) || current.IssueVersion != issueVersion {
			return ErrNotReady
		}
		active, err := store.KeyActive(ctx, nodeID, current.ReceiptKey)
		if err != nil || !active {
			return ErrNotReady
		}
		changed, err := store.CompleteIssue(ctx, certificatestore.Issued{ID: request.CertificateID, Chain: result.CertificateChainPEM, IssueVersion: issueVersion, Serial: leaf.SerialNumber.String(), NotBefore: leaf.NotBefore.UTC(), NotAfter: leaf.NotAfter.UTC(), At: now})
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

func (s *Service) prepareIssue(ctx context.Context, request *IssueRequest) (workspaceID, nodeID uuid.UUID, csrDER []byte, issueVersion int64, err error) {
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
		csrDigest := sha256.Sum256(csrDER)
		if !stored.ReceiptCurrent || len(stored.ReceiptDigest) != sha256.Size || len(stored.ReceiptKey) == 0 || !bytes.Equal(stored.CSRDigest, csrDigest[:]) {
			return ErrNotReady
		}
		active, err := store.KeyActive(ctx, nodeID, stored.ReceiptKey)
		if err != nil || !active {
			return ErrNotReady
		}
		switch stored.State {
		case "csr_ready":
			issueVersion = stored.Version
			now := s.now()
			if err := store.StartIssue(ctx, certificatestore.Signing{ID: request.CertificateID, ActorID: request.ActorIdentityID, Version: issueVersion, At: now}); err != nil {
				return err
			}
			return audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorIdentityID.String(), SessionID: &request.ActorSessionID, Action: "certificate.issue", ResourceType: "certificate", ResourceID: request.CertificateID, NodeID: &nodeID, RequestID: request.RequestID, Result: "intent", Reason: request.Reason, AfterSummary: json.RawMessage(fmt.Sprintf(`{"csr_sha256":%q,"state":"signing"}`, hex.EncodeToString(csrDigest[:]))), At: now})
		case "signing", "signer_unavailable":
			if stored.IssueVersion == nil || *stored.IssueVersion < 1 {
				return ErrNotReady
			}
			issueVersion = *stored.IssueVersion
			if stored.ActorID == nil || *stored.ActorID != request.ActorIdentityID {
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
		return uuid.Nil, uuid.Nil, nil, 0, err
	}
	return
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
	if serialNumber == "" || (!existingRequest && state == "revoked") {
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
	if errors.Is(err, operations.ErrStaleRevision) {
		return operations.Operation{}, false, ErrNotReady
	}
	if err != nil {
		return operations.Operation{}, false, err
	}
	if replay && op.State == "expired" {
		return op, true, ErrNotReady
	}
	operationID, err := uuid.Parse(op.ID)
	if err != nil {
		return op, replay, err
	}
	pending, err := s.pendingRevocations(ctx, &operationID)
	if err != nil || len(pending) == 0 {
		return op, replay, err
	}
	return op, replay, s.revokeAtSigner(ctx, pending[0])
}

func (s *Service) signingReceipt(ctx context.Context, certificateID, nodeID uuid.UUID, csrDER []byte, issueVersion int64) (certificatestore.IssueReceipt, error) {
	var approved certificatestore.IssueReceipt
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		approved, err = store.SigningReceipt(ctx, certificateID)
		return err
	})
	if err != nil || approved.NodeID != nodeID || !approved.Current || !approved.KeyActive || approved.IssueVersion != issueVersion || !bytes.Equal(approved.CSR, csrDER) {
		return approved, ErrNotReady
	}
	return approved, nil
}

func (s *Service) pendingRevocations(ctx context.Context, operationID *uuid.UUID) ([]certificatestore.PendingRevocation, error) {
	var pending []certificatestore.PendingRevocation
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		pending, err = store.PendingRevocations(ctx, operationID)
		return err
	})
	return pending, err
}

func (s *Service) reconcileRevocations(ctx context.Context) error {
	if s.signer == nil {
		return nil
	}
	pending, err := s.pendingRevocations(ctx, nil)
	if err != nil {
		return err
	}
	for _, item := range pending {
		if err := s.revokeAtSigner(ctx, item); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.WarnContext(ctx, "certificate revocation reconciliation failed", "operation_id", item.OperationID, "error", err)
		}
	}
	return s.reconcileExpiredRevocations(ctx)
}

func (s *Service) reconcileExpiredRevocations(ctx context.Context) error {
	var pending []certificatestore.PendingRevocation
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		pending, err = store.ExpiredRevocations(ctx)
		return err
	})
	if err != nil {
		return err
	}
	for _, item := range pending {
		envelope, certificateID, err := s.authorizedRevocation(item)
		var version uint64
		if err != nil {
			slog.WarnContext(ctx, "invalid expired certificate revocation", "operation_id", item.OperationID)
		} else {
			version = envelope.GetCertificateRevoke().GetCertificateVersion()
		}
		if err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
			store, err := certificatestore.FromTransaction(tx)
			if err != nil {
				return err
			}
			return store.ExpireRevocation(ctx, item.CommandID, certificateID, version, item.Envelope, s.now())
		}); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.WarnContext(ctx, "record expired certificate revocation", "operation_id", item.OperationID, "error", err)
		}
	}
	return nil
}

func (s *Service) authorizedRevocation(item certificatestore.PendingRevocation) (*agentv1.CommandEnvelope, uuid.UUID, error) {
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(item.Envelope, &envelope); err != nil {
		return nil, uuid.Nil, ErrInvalid
	}
	revoke := envelope.GetCertificateRevoke()
	if revoke == nil || envelope.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY ||
		!bytes.Equal(envelope.GetNodeId(), item.NodeID[:]) ||
		!bytes.Equal(envelope.GetOperationId(), item.OperationID[:]) ||
		!bytes.Equal(envelope.GetCommandId(), item.CommandID[:]) ||
		s.grantSigner == nil || envelope.GetAuthorization().GetKeyId() != s.grantSigner.KeyID() {
		return nil, uuid.Nil, ErrInvalid
	}
	certificateID, err := uuid.FromBytes(revoke.GetCertificateId())
	if err != nil || certificateID.Version() != 7 {
		return nil, uuid.Nil, ErrInvalid
	}
	claims, err := commandauth.ClaimsFromEnvelopeV1(&envelope)
	if err != nil {
		return nil, uuid.Nil, ErrInvalid
	}
	digest, err := semanticpayload.HashV2(&envelope)
	if err != nil || digest != claims.SemanticPayloadSHA256 {
		return nil, uuid.Nil, ErrInvalid
	}
	proof, err := commandauth.CanonicalV1(claims)
	if err != nil || !ed25519.Verify(s.grantSigner.PublicKey(), proof, envelope.GetAuthorization().GetSignature()) {
		return nil, uuid.Nil, ErrInvalid
	}
	return &envelope, certificateID, nil
}

func (s *Service) revokeAtSigner(ctx context.Context, item certificatestore.PendingRevocation) error {
	envelope, certificateID, err := s.authorizedRevocation(item)
	if err != nil {
		return err
	}
	revoke := envelope.GetCertificateRevoke()
	callCtx, stopCall := context.WithDeadline(ctx, envelope.GetExpiresAt().AsTime())
	defer stopCall()
	var serialNumber string
	if err := database.Within(callCtx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		serialNumber, err = store.RevocationSerial(callCtx, certificateID, item.WorkspaceID, item.NodeID, revoke.GetCertificateVersion())
		return err
	}); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return ErrNotReady
		}
		return err
	}
	err = s.signer.Revoke(callCtx, RevokeSignerRequest{CertificateID: certificateID, SerialNumber: serialNumber, Reason: revoke.GetReason()})
	settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err != nil {
		if updateErr := database.Within(settle, s.backend, database.ReadCommitted, func(tx database.Tx) error {
			store, storeErr := certificatestore.FromTransaction(tx)
			if storeErr != nil {
				return storeErr
			}
			return store.RevocationUnknown(settle, item.OperationID, certificateID, s.now())
		}); updateErr != nil {
			slog.WarnContext(ctx, "record failed Signer revocation", "operation_id", item.OperationID, "error", updateErr)
		}
		return fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
	}
	return s.releaseOperation(settle, item.OperationID, certificateID)
}
