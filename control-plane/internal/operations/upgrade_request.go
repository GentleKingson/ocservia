package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac/rbacstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/google/uuid"
)

var (
	ErrUpgradeArchitectureUnknown = errors.New("node package architecture is unknown")
	ErrUpgradeReleaseNotTrusted   = errors.New("agent release is not trusted")
	ErrUpgradeTargetNotNewer      = errors.New("agent upgrade target is not newer")
)

// AgentUpgradeTarget is a value snapshot of an operator-provisioned release,
// not authorization to execute it. No caller-supplied package identity is used.
type AgentUpgradeTarget struct {
	Version       string
	Architecture  string
	PackageSHA256 [32]byte
}

// PrepareAgentUpgrade preserves single-node admission, not rollout eligibility.
// CreateSynthetic still owns the locked revision/capability/approval checks and
// idempotent intent transaction. In particular, do not replace ExpectedVersion
// with a newly read node version here: that would change replay identity.
func (s *Service) PrepareAgentUpgrade(ctx context.Context, workspaceID, nodeID uuid.UUID, version string) (AgentUpgradeTarget, error) {
	target, observedVersion, err := s.agentUpgradeTarget(ctx, workspaceID, nodeID, version)
	if err != nil {
		return AgentUpgradeTarget{}, err
	}
	if observedVersion == "" || telemetry.ClassifyAgentVersion(observedVersion, target.Version) != telemetry.AgentVersionStateUpgradeAvailable {
		return AgentUpgradeTarget{}, ErrUpgradeTargetNotNewer
	}
	return target, nil
}

// AgentUpgradeApprovalBinding pins the same trusted target as preparation.
// Approval historically allows an equal/older target or an unknown observed
// version; it is not a promise that subsequent execution will be eligible.
func (s *Service) AgentUpgradeApprovalBinding(ctx context.Context, workspaceID, nodeID uuid.UUID, version string) ([]byte, json.RawMessage, error) {
	target, _, err := s.agentUpgradeTarget(ctx, workspaceID, nodeID, version)
	if err != nil {
		return nil, nil, err
	}
	hash, summary := approvals.AgentUpgradeBinding(nodeID, target.Version, target.PackageSHA256[:], target.Architecture)
	return hash, summary, nil
}

func (s *Service) agentUpgradeTarget(ctx context.Context, workspaceID, nodeID uuid.UUID, version string) (AgentUpgradeTarget, string, error) {
	version = strings.TrimSpace(version)
	if !semanticpayload.ValidAgentUpgradeTargetVersion(version) {
		return AgentUpgradeTarget{}, "", ErrInvalidRequest
	}
	store, err := rbacstore.From(s.backend)
	if err != nil {
		return AgentUpgradeTarget{}, "", fmt.Errorf("%w: %w", ErrNodeUnavailable, err)
	}
	var nodeWorkspace uuid.UUID
	var architecture, observedVersion string
	if err := store.UpgradeNode(ctx, nodeID).Scan(&nodeWorkspace, &architecture, &observedVersion); err != nil {
		return AgentUpgradeTarget{}, "", fmt.Errorf("%w: %w", ErrNodeUnavailable, err)
	}
	if nodeWorkspace != workspaceID {
		return AgentUpgradeTarget{}, "", ErrNodeUnavailable
	}
	if architecture == "" {
		return AgentUpgradeTarget{}, "", ErrUpgradeArchitectureUnknown
	}
	digest, trusted := s.releaseCatalog.Lookup(version, architecture)
	if !trusted {
		return AgentUpgradeTarget{}, "", ErrUpgradeReleaseNotTrusted
	}
	return AgentUpgradeTarget{Version: version, Architecture: architecture, PackageSHA256: digest}, observedVersion, nil
}
