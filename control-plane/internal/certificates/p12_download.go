package certificates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func (s *Service) ArtifactResource(ctx context.Context, id uuid.UUID) (workspaceID, nodeID uuid.UUID, err error) {
	err = s.withArtifactStore(ctx, func(_ database.Tx, store certificatestore.ArtifactStore) error {
		workspaceID, nodeID, err = store.Resource(ctx, id)
		return err
	})
	return
}

func (s *Service) OpenArtifact(ctx context.Context, id uuid.UUID, token string, subject, sessionID uuid.UUID) (ArtifactDownload, error) {
	if id == uuid.Nil || subject == uuid.Nil || sessionID == uuid.Nil || len(token) != 43 || s.artifacts == nil || s.grantSigner == nil {
		return ArtifactDownload{}, ErrArtifactDenied
	}
	tokenHash := sha256.Sum256([]byte(token))
	grantID := uuid.Must(uuid.NewV7())
	var eligible certificatestore.Eligible
	var grant *agentv1.ArtifactGrantV1
	err := s.withArtifactStore(ctx, func(tx database.Tx, store certificatestore.ArtifactStore) error {
		workspaceID, _, err := store.Resource(ctx, id)
		if errors.Is(err, database.ErrNotFound) {
			return ErrArtifactDenied
		}
		if err != nil {
			return err
		}
		if err := auth.AuthorizeMutationTx(ctx, tx, subject, sessionID, workspaceID); err != nil {
			return err
		}
		if err = store.LockCapacity(ctx); err != nil {
			return err
		}
		active, err := store.Active(ctx)
		if err != nil {
			return err
		}
		if active >= 4 {
			return ErrArtifactCapacity
		}
		eligible, err = store.Eligible(ctx, id, tokenHash[:])
		if err != nil {
			return ErrArtifactDenied
		}
		if eligible.RequesterID != subject {
			return ErrArtifactDenied
		}
		issuedAt := s.now().UTC()
		expires := issuedAt.Add(time.Minute)
		for _, deadline := range []time.Time{eligible.ArtifactExpires, eligible.CertificateExpires} {
			if deadline.IsZero() {
				return ErrArtifactDenied
			}
			if deadline.Before(expires) {
				expires = deadline
			}
		}
		grant, err = s.grantSigner.IssueArtifactGrant(eligible.NodeID, id, eligible.CertificateID, eligible.CertificateVersion, eligible.OperationID, eligible.RequesterID.String(), uint64(eligible.Size), grantID, issuedAt, expires)
		if err != nil {
			return err
		}
		return store.Lease(ctx, id, grantID, subject, expires)
	})
	if err != nil {
		return ArtifactDownload{}, err
	}
	reader, err := s.artifacts.FetchArtifact(ctx, grant)
	if err != nil {
		_ = s.AbortArtifact(context.WithoutCancel(ctx), id, grantID)
		return ArtifactDownload{}, err
	}
	return ArtifactDownload{Reader: reader, ExpectedSHA256: eligible.Digest, Size: eligible.Size, NodeID: eligible.NodeID, GrantID: grantID, Grant: grant}, nil
}

func (s *Service) CompleteArtifact(ctx context.Context, id, grantID uuid.UUID, grant *agentv1.ArtifactGrantV1, digest []byte, size int64, actorID, sessionID uuid.UUID, requestID string) error {
	if s.artifacts == nil || grant == nil || actorID == uuid.Nil || sessionID == uuid.Nil || requestID == "" || len(requestID) > 128 || grant.GetVersion() != 1 || grant.GetPurpose() != "certificate_p12" || grant.GetCertificateVersion() == 0 || grant.GetAuthorizedSubject() != actorID.String() || !bytes.Equal(grant.GetArtifactId(), id[:]) || !bytes.Equal(grant.GetGrantId(), grantID[:]) || len(digest) != sha256.Size || size < 1 || uint64(size) != grant.GetMaxBytes() {
		return ErrArtifactDenied
	}
	nodeID, nodeErr := uuid.FromBytes(grant.GetNodeId())
	certificateID, certificateErr := uuid.FromBytes(grant.GetCertificateId())
	operationID, operationErr := uuid.FromBytes(grant.GetOperationId())
	if nodeErr != nil || certificateErr != nil || operationErr != nil || nodeID.Version() != 7 || certificateID.Version() != 7 || operationID.Version() != 7 {
		return ErrArtifactDenied
	}
	grantBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(grant)
	if err != nil || len(grantBytes) == 0 || len(grantBytes) > 4096 {
		return ErrArtifactDenied
	}
	consumption := certificatestore.Consumption{ID: id, GrantID: grantID, ActorID: actorID, SessionID: sessionID, NodeID: nodeID, CertificateID: certificateID, OperationID: operationID, CertificateVersion: grant.GetCertificateVersion(), Grant: grantBytes, Digest: digest, Size: size, RequestID: requestID}
	exactReplay := false
	err = s.withArtifactStore(ctx, func(tx database.Tx, store certificatestore.ArtifactStore) error {
		workspaceID, _, err := store.Resource(ctx, id)
		if errors.Is(err, database.ErrNotFound) {
			return ErrArtifactDenied
		}
		if err != nil {
			return err
		}
		if err := auth.AuthorizeMutationTx(ctx, tx, actorID, sessionID, workspaceID); err != nil {
			return err
		}
		changed, err := store.StartConsumption(ctx, consumption)
		if err != nil {
			return err
		}
		if changed {
			return nil
		}
		exactReplay, err = store.ExactReplay(ctx, consumption)
		if err != nil {
			return err
		}
		if !exactReplay {
			return ErrArtifactDenied
		}
		return nil
	})
	if err != nil {
		return err
	}
	if exactReplay {
		return nil
	}
	if err := s.artifacts.ConsumeArtifact(ctx, grant, digest, size); err != nil {
		return err
	}
	result, err := s.finalizeArtifactConsumption(ctx, id, grantID)
	if err != nil {
		return err
	}
	switch result {
	case artifactConsumptionFinalized, artifactConsumptionAlreadyFinalized:
		return nil
	default:
		return ErrArtifactDenied
	}
}

type artifactConsumptionFinalization uint8

const (
	artifactConsumptionFinalized artifactConsumptionFinalization = iota
	artifactConsumptionAlreadyFinalized
	artifactConsumptionRevoked
	artifactConsumptionExpired
)

func (s *Service) finalizeArtifactConsumption(ctx context.Context, id, grantID uuid.UUID) (artifactConsumptionFinalization, error) {
	result := artifactConsumptionFinalized
	err := s.withArtifactStore(ctx, func(tx database.Tx, store certificatestore.ArtifactStore) error {
		v, err := store.Finalize(ctx, id, grantID)
		if errors.Is(err, database.ErrNotFound) {
			state, err := store.State(ctx, id, grantID)
			if err != nil {
				return ErrArtifactDenied
			}
			switch state {
			case "consumed":
				result = artifactConsumptionAlreadyFinalized
			case "revoked":
				result = artifactConsumptionRevoked
			case "expired":
				result = artifactConsumptionExpired
			default:
				return ErrArtifactDenied
			}
			return nil
		}
		if err != nil {
			return err
		}
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: v.WorkspaceID, ActorType: "user", ActorID: v.ActorID.String(), SessionID: &v.SessionID, Action: "certificate.p12.download", ResourceType: "artifact", ResourceID: id, NodeID: &v.NodeID, RequestID: v.RequestID, Result: "succeeded", AfterSummary: json.RawMessage(fmt.Sprintf(`{"certificate_id":%q,"sha256":%q,"size":%d}`, v.CertificateID, hex.EncodeToString(v.Digest), v.Size)), At: s.now()})
	})
	return result, err
}

func (s *Service) reconcileConsumingArtifacts(ctx context.Context) error {
	var records []certificatestore.PendingConsumption
	err := s.withArtifactStore(ctx, func(_ database.Tx, store certificatestore.ArtifactStore) error {
		var err error
		records, err = store.PendingConsumptions(ctx)
		return err
	})
	if err != nil {
		return err
	}
	for _, record := range records {
		item := pendingArtifactConsumption{id: record.ID, grantID: record.GrantID, grant: &agentv1.ArtifactGrantV1{}, digest: record.Digest, size: record.Size, actorID: record.ActorID, expiresAt: record.ExpiresAt}
		err := proto.Unmarshal(record.Grant, item.grant)
		nodeID, nodeErr := uuid.FromBytes(item.grant.GetNodeId())
		if err != nil || nodeErr != nil || nodeID == uuid.Nil || item.grantID == uuid.Nil || item.actorID == uuid.Nil ||
			item.size <= 0 || item.expiresAt.IsZero() || item.grant.GetExpiresAt().CheckValid() != nil ||
			!bytes.Equal(item.grant.GetArtifactId(), item.id[:]) || !bytes.Equal(item.grant.GetGrantId(), item.grantID[:]) ||
			item.grant.GetAuthorizedSubject() != item.actorID.String() || item.grant.GetMaxBytes() != uint64(item.size) {
			slog.ErrorContext(ctx, "invalid pending artifact consumption", "artifact_id", record.ID)
			err = s.withArtifactStore(ctx, func(_ database.Tx, store certificatestore.ArtifactStore) error {
				return store.FailConsumption(ctx, record.ID, record.GrantID, record.Grant)
			})
		} else {
			err = s.reconcileArtifactConsumption(ctx, item)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.WarnContext(ctx, "artifact consumption reconciliation failed", "artifact_id", record.ID, "error", err)
		}
	}
	return nil
}

type pendingArtifactConsumption struct {
	id, grantID uuid.UUID
	grant       *agentv1.ArtifactGrantV1
	digest      []byte
	size        int64
	actorID     uuid.UUID
	expiresAt   time.Time
}

func (s *Service) reconcileArtifactConsumption(ctx context.Context, item pendingArtifactConsumption) error {
	consumed, confirmErr := s.artifacts.ConfirmArtifactConsumed(ctx, item.grant, item.digest, item.size)
	if confirmErr != nil {
		// Reconciliation is item-scoped. A disconnected node or restarting
		// Agent must not turn one durable artifact into a global scheduler
		// failure; exact evidence remains in consuming for the next pass.
		return confirmErr
	}
	if !consumed && item.grant.GetExpiresAt().AsTime().After(s.now()) {
		if err := s.artifacts.ConsumeArtifact(ctx, item.grant, item.digest, item.size); err != nil {
			return err
		}
		consumed = true
	}
	if consumed {
		// Revocation and expiry may win the terminal transition after root
		// confirmation. They are benign for reconciliation, but the explicit
		// result prevents an in-flight HTTP request from treating them as a
		// successful delivery.
		if _, err := s.finalizeArtifactConsumption(ctx, item.id, item.grantID); err != nil {
			return err
		}
	} else {
		now := s.now().UTC()
		if err := s.withArtifactStore(ctx, func(_ database.Tx, store certificatestore.ArtifactStore) error {
			return store.ResetConsumption(ctx, item.id, item.grantID, !item.expiresAt.After(now))
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) AbortArtifact(ctx context.Context, id, grantID uuid.UUID) error {
	// Preserve the durable lease until its bounded expiry. A transport failure
	// must not make a second concurrently valid grant immediately issuable.
	return s.withArtifactStore(ctx, func(_ database.Tx, store certificatestore.ArtifactStore) error {
		return store.Abort(ctx, id, grantID)
	})
}
