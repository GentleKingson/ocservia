package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
)

var (
	ErrUpgradeArchitectureUnknown = errors.New("node package architecture is unknown")
	ErrUpgradeReleaseNotTrusted   = errors.New("agent release is not trusted")
)

// AgentUpgradeTarget is a value snapshot of an operator-provisioned release,
// not authorization to execute it. No caller-supplied package identity is used.
type AgentUpgradeTarget struct {
	Version       string
	Architecture  string
	PackageSHA256 [32]byte
}

// PrepareAgentUpgrade preserves single-node admission, not rollout eligibility.
// Create still owns the locked revision/capability checks and idempotent intent transaction.
func (s *Service) PrepareAgentUpgrade(ctx context.Context, workspaceID, nodeID uuid.UUID, version string) (AgentUpgradeTarget, error) {
	version = strings.TrimSpace(version)
	if !semanticpayload.ValidAgentUpgradeTargetVersion(version) {
		return AgentUpgradeTarget{}, ErrInvalidRequest
	}
	if s.backend == nil {
		return AgentUpgradeTarget{}, fmt.Errorf("%w: %w", ErrNodeUnavailable, database.ErrUnsupported)
	}
	var nodeWorkspace uuid.UUID
	var architecture string
	if err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		nodeWorkspace, architecture, _, err = store.UpgradeNode(ctx, nodeID)
		return err
	}); err != nil {
		return AgentUpgradeTarget{}, fmt.Errorf("%w: %w", ErrNodeUnavailable, err)
	}
	if nodeWorkspace != workspaceID {
		return AgentUpgradeTarget{}, ErrNodeUnavailable
	}
	if architecture == "" {
		return AgentUpgradeTarget{}, ErrUpgradeArchitectureUnknown
	}
	digest, trusted := s.releaseCatalog.Lookup(version, architecture)
	if !trusted {
		return AgentUpgradeTarget{}, ErrUpgradeReleaseNotTrusted
	}
	return AgentUpgradeTarget{Version: version, Architecture: architecture, PackageSHA256: digest}, nil
}
