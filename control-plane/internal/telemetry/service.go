package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	platformtelemetry "github.com/GentleKingson/ocservia/control-plane/internal/platform/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
	"github.com/google/uuid"
)

const (
	MaxBatchBytes       = 512 << 10
	MaxManagedResources = 384
	MaxReportedGroups   = MaxManagedResources * 2
	OfflineAfter        = 90 * time.Second
	MaxTelemetryAge     = 14 * 24 * time.Hour
	MaxTelemetrySkew    = 5 * time.Minute
)

// ErrInvalidTelemetry identifies wire content that cannot become valid by
// retrying it. Database and transaction failures are deliberately not wrapped.
var ErrInvalidTelemetry = errors.New("telemetry payload is permanently invalid")

var (
	ErrInvalidMetric     = errors.New("metric is invalid")
	ErrInvalidResolution = errors.New("resolution is invalid")
)

var allowedMetrics = map[string]bool{
	"cpu_usage_ratio": true, "memory_used_bytes": true,
	"network_rx_bytes": true, "network_tx_bytes": true,
	"session_count": true, "connection_rtt_ms": true,
}

type Snapshot struct {
	ObservedAt     time.Time       `json:"observed_at"`
	BootID         string          `json:"boot_id"`
	AgentInstance  uuid.UUID       `json:"agent_instance_id"`
	AgentVersion   string          `json:"agent_version"`
	OcservVersion  string          `json:"ocserv_version"`
	OSRelease      string          `json:"os_release"`
	Architecture   string          `json:"architecture,omitempty"`
	Ocserv         json.RawMessage `json:"ocserv"`
	System         json.RawMessage `json:"system"`
	Path           json.RawMessage `json:"path"`
	Dropped        DropCounters    `json:"dropped"`
	UpgradeResults []UpgradeResult `json:"upgrade_results,omitempty"`
}

// UpgradeResult is one bounded durable local upgrader outcome reported
// read-only by the Agent through its heartbeat.
type UpgradeResult struct {
	OperationID   uuid.UUID                        `json:"operation_id"`
	State         string                           `json:"state"`
	TargetVersion string                           `json:"target_version"`
	CompletedAt   time.Time                        `json:"completed_at"`
	Detail        string                           `json:"detail,omitempty"`
	Proof         *agentv1.AgentUpgradeResultProof `json:"privileged_result_proof,omitempty"`
}

type DropCounters struct {
	Security  uint64 `json:"security"`
	Health    uint64 `json:"health"`
	Aggregate uint64 `json:"aggregate"`
	Raw       uint64 `json:"raw"`
}

type Session = telemetrystore.Session
type IPBan = telemetrystore.IPBan
type ReadSession = telemetrystore.ReadSession
type User = telemetrystore.User

type Group struct {
	Name     string   `json:"name"`
	Members  []string `json:"members"`
	Revision uint64   `json:"revision"`
}

type Sample = telemetrystore.Sample
type SecurityEvent = telemetrystore.SecurityEvent

type Batch struct {
	ID       uuid.UUID       `json:"id"`
	NodeID   uuid.UUID       `json:"node_id"`
	Sequence uint64          `json:"sequence"`
	Kind     string          `json:"kind"`
	Snapshot Snapshot        `json:"snapshot"`
	Sessions []Session       `json:"sessions"`
	IPBans   []IPBan         `json:"ip_bans"`
	Samples  []Sample        `json:"samples"`
	Security []SecurityEvent `json:"security_events"`
	Users    []User          `json:"users"`
	Groups   []Group         `json:"groups"`
}

type Node struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Version         int64      `json:"version"`
	ConfigRevision  int64      `json:"config_revision"`
	TrustStatus     string     `json:"trust_status"`
	ConnectionState string     `json:"connection_state"`
	Freshness       string     `json:"freshness"`
	ObservedAt      *time.Time `json:"observed_at,omitempty"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	BootID          string     `json:"boot_id,omitempty"`
	AgentInstanceID string     `json:"agent_instance_id,omitempty"`
	AgentVersion    string     `json:"agent_version,omitempty"`
	// AgentVersionState and RecommendedAgentVersion are derived at read time
	// from AgentVersion and the configured recommendation; they are never
	// persisted.
	AgentVersionState       string `json:"agent_version_state"`
	RecommendedAgentVersion string `json:"recommended_agent_version,omitempty"`
	Architecture            string `json:"architecture,omitempty"`
	// AgentUpgradeEligible is a read-time derivation: an online fresh node
	// with the upgrade capability, a
	// trusted release for its architecture, and no conflicting active upgrade.
	AgentUpgradeEligible bool            `json:"agent_upgrade_eligible"`
	OcservVersion        string          `json:"ocserv_version,omitempty"`
	OSRelease            string          `json:"os_release,omitempty"`
	Ocserv               json.RawMessage `json:"ocserv,omitempty"`
	System               json.RawMessage `json:"system,omitempty"`
	Path                 json.RawMessage `json:"path,omitempty"`
	Dropped              DropCounters    `json:"dropped"`
	SessionCount         int             `json:"session_count"`

	// Only detail reads derive action availability; list reads omit it.
	EffectiveActions map[string]ActionAvailability `json:"effective_actions,omitempty"`
}

type HistoryPoint = telemetrystore.Point

func (s *Service) ListIPBans(ctx context.Context, nodeID uuid.UUID, limit int) ([]IPBan, error) {
	if nodeID == uuid.Nil || limit < 1 || limit > 200 {
		return nil, errors.New("IP ban query is invalid")
	}
	var result []IPBan
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		result, err = telemetrystore.FromTransaction(tx).IPBans(ctx, nodeID, limit)
		return err
	})
	return result, err
}

type Service struct {
	backend                 database.Backend
	now                     func() time.Time
	recommendedAgentVersion string
	agentUpgradeCatalog     *releasecatalog.Catalog
	maintenanceMu           sync.Mutex
	partitionsEnsuredAt     time.Time
}

// New enables ingestion, read models and maintenance through domain stores.
func New(backend database.Backend) *Service { return &Service{backend: backend, now: time.Now} }

// NewWithRecommendedAgentVersion builds the read model with the
// operator-configured recommended agent version used to derive per-node
// agent version state.
func NewWithRecommendedAgentVersion(backend database.Backend, recommendedAgentVersion string) *Service {
	return &Service{backend: backend, now: time.Now, recommendedAgentVersion: recommendedAgentVersion}
}

// EnableAgentUpgradeEligibility installs the trusted release catalog used to
// derive whether a node currently offers the single-node upgrade workflow.
func (s *Service) EnableAgentUpgradeEligibility(catalog *releasecatalog.Catalog) {
	s.agentUpgradeCatalog = catalog
}

func (s *Service) applyAgentVersionState(node *Node) {
	node.RecommendedAgentVersion = s.recommendedAgentVersion
	node.AgentVersionState = ClassifyAgentVersion(node.AgentVersion, s.recommendedAgentVersion)
}

func (s *Service) ListNodesInWorkspace(ctx context.Context, workspaceID, after uuid.UUID, limit int) ([]Node, bool, error) {
	if limit < 1 || limit > 200 {
		return nil, false, errors.New("node page size must be between 1 and 200")
	}
	var stored []telemetrystore.Node
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		stored, err = telemetrystore.FromTransaction(tx).Nodes(ctx, workspaceID, after, limit+1)
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("list nodes: %w", err)
	}
	result := []Node{}
	for _, v := range stored {
		node := readNode(v, s.now())
		s.applyAgentVersionState(&node)
		s.applyAgentUpgradeEligibility(&node, v.UpgradeEligible)
		result = append(result, node)
	}
	hasMore := len(result) > limit
	if hasMore {
		result = result[:limit]
	}
	return result, hasMore, nil
}

func (s *Service) GetNode(ctx context.Context, id uuid.UUID) (Node, error) {
	var stored telemetrystore.Node
	var actions map[string]ActionAvailability
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		stored, err = telemetrystore.FromTransaction(tx).Node(ctx, id)
		if err != nil {
			return err
		}
		actions, err = nodeActions(ctx, enrollmentstore.Enrollment(tx), id, stored.Status)
		return err
	})
	if err != nil {
		return Node{}, err
	}
	node := readNode(stored, s.now())
	node.EffectiveActions = actions
	s.applyAgentVersionState(&node)
	s.applyAgentUpgradeEligibility(&node, stored.UpgradeEligible)
	return node, nil
}

// applyAgentUpgradeEligibility derives the single-node upgrade workflow gate
// from durable state: an online fresh node with the approved upgrade
// capability, a trusted release for its architecture, and no conflicting active
// upgrade (computed inside the read query). It is never persisted and fails
// closed when unavailable.
func (s *Service) applyAgentUpgradeEligibility(node *Node, upgradeEligible bool) {
	if s.agentUpgradeCatalog == nil || node.ConnectionState != "online" || node.Freshness != "fresh" || node.Architecture == "" {
		return
	}
	if _, trusted := s.agentUpgradeCatalog.Lookup(node.RecommendedAgentVersion, node.Architecture, node.OSRelease); !trusted {
		return
	}
	node.AgentUpgradeEligible = upgradeEligible
}

func agentUpgradeOutcomeState(state agentv1.AgentUpgradeOutcomeState) string {
	switch state {
	case agentv1.AgentUpgradeOutcomeState_AGENT_UPGRADE_OUTCOME_STATE_SUCCEEDED:
		return "succeeded"
	case agentv1.AgentUpgradeOutcomeState_AGENT_UPGRADE_OUTCOME_STATE_FAILED:
		return "failed"
	case agentv1.AgentUpgradeOutcomeState_AGENT_UPGRADE_OUTCOME_STATE_ROLLED_BACK:
		return "rolled_back"
	default:
		return ""
	}
}

func agentUpgradeOutcomeProtoState(state string) agentv1.AgentUpgradeOutcomeState {
	switch state {
	case "succeeded":
		return agentv1.AgentUpgradeOutcomeState_AGENT_UPGRADE_OUTCOME_STATE_SUCCEEDED
	case "failed":
		return agentv1.AgentUpgradeOutcomeState_AGENT_UPGRADE_OUTCOME_STATE_FAILED
	case "rolled_back":
		return agentv1.AgentUpgradeOutcomeState_AGENT_UPGRADE_OUTCOME_STATE_ROLLED_BACK
	default:
		return agentv1.AgentUpgradeOutcomeState_AGENT_UPGRADE_OUTCOME_STATE_UNSPECIFIED
	}
}

func upgradeResultCompletedAtMatches(proof *agentv1.AgentUpgradeResultProof, completedAt time.Time) bool {
	if proof == nil || completedAt.IsZero() || completedAt.UnixMilli() < 0 {
		return false
	}
	return completedAt.Equal(time.UnixMilli(int64(proof.GetCompletedUnixMs())).UTC())
}

func readNode(v telemetrystore.Node, now time.Time) Node {
	n := Node{ID: v.ID.String(), Name: v.Name, Version: v.Version, ConfigRevision: v.ConfigRevision, TrustStatus: v.Status,
		BootID: v.BootID, AgentInstanceID: v.InstanceID, AgentVersion: v.AgentVersion,
		OcservVersion: v.OcservVersion, OSRelease: v.OSRelease, Architecture: v.Architecture,
		Ocserv: v.Ocserv, System: v.System, Path: v.Path, SessionCount: v.Sessions,
		ObservedAt: v.ObservedAt, LastHeartbeatAt: v.Heartbeat,
		Dropped: DropCounters{Security: v.Security, Health: v.Health, Aggregate: v.Aggregate, Raw: v.Raw}}
	if v.Heartbeat == nil {
		n.Freshness = "never"
		n.ConnectionState = "offline"
	} else if !v.Heartbeat.Before(now.Add(-OfflineAfter)) {
		n.Freshness = "fresh"
		n.ConnectionState = "online"
	} else {
		n.Freshness = "stale"
		n.ConnectionState = "offline"
	}
	if v.OwnerActive != nil && !*v.OwnerActive {
		n.ConnectionState = "offline"
	}
	return n
}

func (s *Service) ListSessions(ctx context.Context, nodeID uuid.UUID, after string, limit int) ([]telemetrystore.ReadSession, bool, error) {
	if limit < 1 || limit > 200 || len(after) > 256 {
		return nil, false, errors.New("session page is invalid")
	}
	var result []telemetrystore.ReadSession
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		result, err = telemetrystore.FromTransaction(tx).Sessions(ctx, nodeID, after, limit+1)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	hasMore := len(result) > limit
	if hasMore {
		result = result[:limit]
	}
	return result, hasMore, nil
}

func (s *Service) History(ctx context.Context, nodeID uuid.UUID, metric, resolution string, since time.Time) ([]HistoryPoint, error) {
	if !allowedMetrics[metric] {
		return nil, ErrInvalidMetric
	}
	if since.IsZero() {
		since = s.now().Add(-24 * time.Hour)
	}
	if resolution != "raw" && resolution != "5m" && resolution != "1h" {
		return nil, ErrInvalidResolution
	}
	var points []HistoryPoint
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		points, err = telemetrystore.FromTransaction(tx).History(ctx, nodeID, metric, resolution, since.UTC())
		return err
	})
	return points, err
}

func (s *Service) Maintain(ctx context.Context) error {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	now := s.now().UTC()
	ensurePartitions := s.partitionsEnsuredAt.IsZero() || now.Sub(s.partitionsEnsuredAt) >= time.Hour || now.Before(s.partitionsEnsuredAt)
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store := telemetrystore.FromTransaction(tx)
		if ensurePartitions {
			if err := store.EnsureUpcomingPartitions(ctx); err != nil {
				return err
			}
		}
		offline, err := store.LockOfflineCandidates(ctx, now.Add(-OfflineAfter))
		if err != nil {
			return err
		}
		for _, id := range offline {
			if err := store.MarkOffline(ctx, id, uuid.Must(uuid.NewV7()), now, platformtelemetry.NewTraceparent()); err != nil {
				return err
			}
		}
		return store.MaintainHistory(ctx, now)
	})
	if err == nil && ensurePartitions {
		s.partitionsEnsuredAt = now
	}
	return err
}
