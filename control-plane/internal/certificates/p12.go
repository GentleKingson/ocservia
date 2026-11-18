package certificates

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
)

func (s *Service) CreateP12(ctx context.Context, request P12Request) (ArtifactGrant, bool, error) {
	if request.CertificateID == uuid.Nil || request.ArtifactRequestID == uuid.Nil || request.CertificateVersion < 1 || request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil || request.ExpectedVersion < 1 || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.Reason) == "" || request.RequestID == "" {
		return ArtifactGrant{}, false, ErrInvalid
	}
	if s.sealer == nil {
		return ArtifactGrant{}, false, ErrSignerUnavailable
	}
	var eligible certificatestore.ActionBinding
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		eligible, err = store.P12Eligible(ctx, request.CertificateID)
		if err != nil {
			return err
		}
		return auth.AuthorizeMutationTx(ctx, tx, request.ActorIdentityID, request.ActorSessionID, eligible.WorkspaceID)
	})
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return ArtifactGrant{}, false, ErrNotReady
		}
		return ArtifactGrant{}, false, err
	}
	workspaceID, nodeID, chain, certificateVersion := eligible.WorkspaceID, eligible.NodeID, eligible.Chain, eligible.Version
	if certificateVersion != request.CertificateVersion {
		return ArtifactGrant{}, false, ErrNotReady
	}
	intent, _ := json.Marshal(map[string]any{"certificate_id": request.CertificateID, "certificate_version": request.CertificateVersion, "artifact_request_id": request.ArtifactRequestID, "node_id": nodeID, "expected_version": request.ExpectedVersion, "actor_identity_id": request.ActorIdentityID, "actor_session_id": request.ActorSessionID, "reason": request.Reason})
	intentHash := sha256.Sum256(intent)
	existing, err := s.artifactByRequest(ctx, workspaceID, request.IdempotencyKey, intentHash[:])
	if err == nil {
		if !existing.SameIntent {
			return ArtifactGrant{}, false, operations.ErrIdempotencyConflict
		}
		return s.replayArtifactGrant(ctx, existing)
	}
	if !errors.Is(err, database.ErrNotFound) {
		return ArtifactGrant{}, false, err
	}
	password, token, sealed, err := s.prepareP12Secrets(ctx, nodeID)
	if err != nil {
		return ArtifactGrant{}, false, err
	}
	artifactID := request.ArtifactRequestID
	expiresAt := s.now().Add(10 * time.Minute)
	expiry, err := value.FromTime(expiresAt)
	if err != nil {
		return ArtifactGrant{}, false, err
	}
	tokenHash := sha256.Sum256([]byte(token))
	op, replay, err := s.operations.Create(ctx, operations.CreateRequest{
		NodeID:          nodeID,
		ExpectedVersion: request.ExpectedVersion,
		IdempotencyKey:  request.IdempotencyKey,
		ActorID:         request.ActorIdentityID.String(),
		ActorIdentityID: request.ActorIdentityID,
		ActorSessionID:  request.ActorSessionID,
		Action:          "certificate.private_key.export",
		Reason:          request.Reason,
		RequestID:       request.RequestID,
		Traceparent:     request.Traceparent,
		TTL:             15 * time.Minute,
		Payload: operations.CertificateP12Payload{
			CertificateID:      request.CertificateID,
			CertificateVersion: uint64(certificateVersion),
			CertificateChain:   chain,
			SealedPassword:     sealed,
			ArtifactID:         artifactID,
			ArtifactMetadata:   &operations.ArtifactMetadata{TokenSHA256: tokenHash[:], RequestHash: intentHash[:], ExpiresAt: expiresAt},
		},
	})
	if err != nil {
		if errors.Is(err, operations.ErrIdempotencyConflict) {
			found, lookupErr := s.artifactByRequest(ctx, workspaceID, request.IdempotencyKey, intentHash[:])
			if lookupErr == nil && found.SameIntent {
				return s.replayArtifactGrant(ctx, found)
			}
		}
		return ArtifactGrant{}, false, err
	}
	if replay {
		operationID, err := uuid.Parse(op.ID)
		if err != nil {
			return ArtifactGrant{}, false, err
		}
		var found certificatestore.Artifact
		if err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
			store, err := certificatestore.FromTransaction(tx)
			if err != nil {
				return err
			}
			found, err = store.ArtifactByOperation(ctx, operationID)
			return err
		}); err != nil {
			return ArtifactGrant{}, false, err
		}
		return ArtifactGrant{ArtifactID: found.ID, Operation: op, ExpiresAt: found.ExpiresAt}, true, nil
	}
	return ArtifactGrant{ArtifactID: artifactID, Operation: op, DownloadToken: token, Password: password, ExpiresAt: expiry}, false, nil
}

func (s *Service) artifactByRequest(ctx context.Context, workspace uuid.UUID, key string, hash []byte) (v certificatestore.Artifact, err error) {
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		v, err = store.ArtifactByRequest(ctx, workspace, key, hash)
		return err
	})
	return
}

func (s *Service) prepareP12Secrets(ctx context.Context, nodeID uuid.UUID) (string, string, *agentv1.SealedSecretV1, error) {
	passwordBytes, tokenBytes := make([]byte, 24), make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		return "", "", nil, err
	}
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", "", nil, err
	}
	password := base64.RawURLEncoding.EncodeToString(passwordBytes)
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	for index := range passwordBytes {
		passwordBytes[index] = 0
	}
	sealed, err := s.sealer.Seal(ctx, nodeID, agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD, []byte(password))
	if err != nil {
		return "", "", nil, fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
	}
	var registeredKey bool
	if err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		registeredKey, err = store.SealingKeyExists(ctx, nodeID, uint32(sealed.GetVersion()), sealed.GetKeyId())
		return err
	}); err != nil {
		return "", "", nil, err
	}
	if !registeredKey {
		return "", "", nil, ErrSignerUnavailable
	}
	return password, token, sealed, nil
}

func (s *Service) replayArtifactGrant(ctx context.Context, existing certificatestore.Artifact) (ArtifactGrant, bool, error) {
	op, getErr := s.operations.Get(ctx, existing.OperationID)
	if getErr != nil {
		return ArtifactGrant{}, false, getErr
	}
	return ArtifactGrant{ArtifactID: existing.ID, Operation: op, ExpiresAt: existing.ExpiresAt}, true, nil
}
