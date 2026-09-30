package enrollment

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/google/uuid"
)

var (
	ErrSealingUnavailable = errors.New("verified sealing key source is unavailable")
	ErrSealingBinding     = errors.New("sealing key binding does not match current enrollment")
)

type userPasswordPublicKeySource interface {
	UserPasswordPublicKey(context.Context, uuid.UUID) ([]byte, error)
}

type UserPasswordSealingKey struct {
	WorkspaceID     uuid.UUID `json:"workspace_id"`
	NodeID          uuid.UUID `json:"node_id"`
	EndpointID      string    `json:"endpoint_id"`
	Purpose         string    `json:"purpose"`
	Version         int32     `json:"version"`
	KeyID           string    `json:"key_id"`
	PublicKeySHA256 string    `json:"public_key_sha256"`
	PublicKeyDER    []byte    `json:"public_key_der"`
}

func (s *Service) EnableUserPasswordPublicKeys(source userPasswordPublicKeySource) {
	s.publicKeys = source
}

func (s *Service) UserPasswordSealingKey(ctx context.Context, workspace, nodeID uuid.UUID) (UserPasswordSealingKey, error) {
	if s.publicKeys == nil {
		return UserPasswordSealingKey{}, ErrSealingUnavailable
	}
	data, err := s.publicKeys.UserPasswordPublicKey(ctx, nodeID)
	if err != nil {
		return UserPasswordSealingKey{}, ErrSealingUnavailable
	}
	var key UserPasswordSealingKey
	if len(data) > 16<<10 || json.Unmarshal(data, &key) != nil || key.WorkspaceID != workspace || key.NodeID != nodeID || key.Purpose != "user_password" || key.Version != 1 || key.KeyID == "" || len(key.KeyID) > 128 || len(key.PublicKeyDER) > 1024 {
		return UserPasswordSealingKey{}, ErrSealingBinding
	}
	digest := sha256.Sum256(key.PublicKeyDER)
	if hex.EncodeToString(digest[:]) != key.PublicKeySHA256 {
		return UserPasswordSealingKey{}, ErrSealingBinding
	}
	parsed, err := x509.ParsePKIXPublicKey(key.PublicKeyDER)
	if err != nil {
		return UserPasswordSealingKey{}, ErrSealingBinding
	}
	public, ok := parsed.(*rsa.PublicKey)
	if !ok || public.N.BitLen() < 2048 || public.N.BitLen() > 4096 || public.E != 65537 {
		return UserPasswordSealingKey{}, ErrSealingBinding
	}
	// Read current descriptors after the remote read, so a rotated/revoked
	// enrollment cannot be served from a previously verified Signer binding.
	err = database.Within(ctx, s.backend, database.RepeatableRead, func(tx database.Tx) error {
		store, err := enrollmentstore.Enrollment(tx)
		if err != nil {
			return err
		}
		node, err := store.NodeByID(ctx, nodeID, enrollmentstore.Unlocked)
		if err != nil {
			return err
		}
		if node.WorkspaceID != workspace || (node.Status != "active" && node.Status != "offline") || node.EndpointState != "active" || len(node.Endpoint) != 32 || key.EndpointID != hex.EncodeToString(node.Endpoint) {
			return ErrSealingBinding
		}
		capabilities, err := store.Capabilities(ctx, nodeID, true)
		if err != nil {
			return err
		}
		if !slices.Contains(capabilities, "ocserv.users.write") {
			return ErrSealingBinding
		}
		keys, err := store.SealingKeys(ctx, nodeID)
		if err != nil {
			return err
		}
		for _, enrolled := range keys {
			if enrolled.Purpose == 1 && enrolled.Version == key.Version && enrolled.ID == key.KeyID && slices.Equal(enrolled.Digest, digest[:]) {
				return nil
			}
		}
		return ErrSealingBinding
	})
	if err != nil {
		return UserPasswordSealingKey{}, err
	}
	return key, nil
}
