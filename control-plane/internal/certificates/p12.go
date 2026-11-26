package certificates

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
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
	err := s.withStore(ctx, func(tx database.Tx, store certificatestore.Store) error {
		var err error
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
	payload := operations.CertificateP12Payload{CertificateID: request.CertificateID, CertificateVersion: uint64(certificateVersion), CertificateChain: chain, ArtifactID: request.ArtifactRequestID}
	create := operations.CreateRequest{
		NodeID: nodeID, ExpectedVersion: request.ExpectedVersion, IdempotencyKey: request.IdempotencyKey,
		ActorID: request.ActorIdentityID.String(), ActorIdentityID: request.ActorIdentityID, ActorSessionID: request.ActorSessionID,
		Action: "certificate.private_key.export", Reason: request.Reason, RequestID: request.RequestID,
		Traceparent: request.Traceparent, TTL: 15 * time.Minute,
	}
	existing, err := s.artifactByRequest(ctx, workspaceID, request.IdempotencyKey)
	if err == nil {
		if !operations.P12Intent(create, payload).Matches(existing.RequestHash) {
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
	expiresAt := s.now().UTC().Add(10 * time.Minute)
	tokenHash := sha256.Sum256([]byte(token))
	payload.SealedPassword = sealed
	payload.ArtifactMetadata = &operations.ArtifactMetadata{TokenSHA256: tokenHash[:], ExpiresAt: expiresAt}
	create.Payload = payload
	op, replay, err := s.operations.Create(ctx, create)
	if err != nil {
		return ArtifactGrant{}, false, err
	}
	if replay {
		found, err := s.artifactByRequest(ctx, workspaceID, request.IdempotencyKey)
		if err != nil {
			return ArtifactGrant{}, false, err
		}
		return ArtifactGrant{ArtifactID: found.ID, Operation: op, ExpiresAt: found.ExpiresAt}, true, nil
	}
	return ArtifactGrant{ArtifactID: payload.ArtifactID, Operation: op, DownloadToken: token, Password: password, ExpiresAt: expiresAt}, false, nil
}

func (s *Service) artifactByRequest(ctx context.Context, workspace uuid.UUID, key string) (v certificatestore.Artifact, err error) {
	err = s.withStore(ctx, func(_ database.Tx, store certificatestore.Store) error {
		v, err = store.ArtifactByRequest(ctx, workspace, key)
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
	if err := s.withStore(ctx, func(_ database.Tx, store certificatestore.Store) error {
		var err error
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
