package telemetry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/postgresinput"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetryhistory"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetryread"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetrywrite"
	"github.com/GentleKingson/ocservia/control-plane/internal/userusage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
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

type Session struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	ClientIP    string    `json:"client_ip"`
	ConnectedAt time.Time `json:"connected_at"`
	BytesIn     int64     `json:"bytes_in"`
	BytesOut    int64     `json:"bytes_out"`
}

type IPBan = telemetryread.IPBan

type User struct {
	Username    string `json:"username"`
	Enabled     bool   `json:"enabled"`
	Revision    uint64 `json:"revision"`
	Fingerprint []byte `json:"fingerprint_sha256"`
}

type Group struct {
	Name        string   `json:"name"`
	Members     []string `json:"members"`
	Revision    uint64   `json:"revision"`
	Fingerprint []byte   `json:"fingerprint_sha256"`
}

type Sample struct {
	SampledAt time.Time `json:"sampled_at"`
	Metric    string    `json:"metric"`
	Value     float64   `json:"value"`
}

type SecurityEvent struct {
	ID         uuid.UUID       `json:"id"`
	ObservedAt time.Time       `json:"observed_at"`
	Severity   string          `json:"severity"`
	Type       string          `json:"type"`
	Detail     json.RawMessage `json:"detail"`
}

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
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Version         int64            `json:"version"`
	TrustStatus     string           `json:"trust_status"`
	ConnectionState string           `json:"connection_state"`
	Freshness       string           `json:"freshness"`
	ObservedAt      *value.Timestamp `json:"observed_at,omitempty"`
	LastHeartbeatAt *value.Timestamp `json:"last_heartbeat_at,omitempty"`
	BootID          string           `json:"boot_id,omitempty"`
	AgentInstanceID string           `json:"agent_instance_id,omitempty"`
	AgentVersion    string           `json:"agent_version,omitempty"`
	// AgentVersionState and RecommendedAgentVersion are derived at read time
	// from AgentVersion and the configured recommendation; they are never
	// persisted.
	AgentVersionState       string `json:"agent_version_state"`
	RecommendedAgentVersion string `json:"recommended_agent_version,omitempty"`
	Architecture            string `json:"architecture,omitempty"`
	// AgentUpgradeEligible is a read-time derivation: an upgrade_available
	// version state, an online fresh node with the upgrade capability, a
	// trusted release for its architecture, and no conflicting active upgrade.
	AgentUpgradeEligible bool            `json:"agent_upgrade_eligible"`
	OcservVersion        string          `json:"ocserv_version,omitempty"`
	OSRelease            string          `json:"os_release,omitempty"`
	Ocserv               json.RawMessage `json:"ocserv,omitempty"`
	System               json.RawMessage `json:"system,omitempty"`
	Path                 json.RawMessage `json:"path,omitempty"`
	Dropped              DropCounters    `json:"dropped"`
	SessionCount         int             `json:"session_count"`
}

type HistoryPoint = telemetryhistory.Point

func (s *Service) ListIPBans(ctx context.Context, nodeID uuid.UUID, limit int) ([]IPBan, error) {
	if nodeID == uuid.Nil || limit < 1 || limit > 200 {
		return nil, errors.New("IP ban query is invalid")
	}
	var result []IPBan
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := telemetryread.From(tx)
		if err != nil {
			return err
		}
		result, err = store.IPBans(ctx, nodeID, limit)
		return err
	})
	return result, err
}

type Service struct {
	backend                 database.Backend
	now                     func() time.Time
	recommendedAgentVersion string
	agentUpgradeCatalog     *releasecatalog.Catalog
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{backend: postgres.WrapPool(pool), now: time.Now}
}

// NewBackend enables ingestion, read models and maintenance through domain stores.
func NewBackend(backend database.Backend) *Service { return &Service{backend: backend, now: time.Now} }

// NewWithRecommendedAgentVersion builds the read model with the
// operator-configured recommended agent version used to derive per-node
// agent version state.
func NewWithRecommendedAgentVersion(pool *pgxpool.Pool, recommendedAgentVersion string) *Service {
	return NewWithRecommendedAgentVersionBackend(postgres.WrapPool(pool), recommendedAgentVersion)
}

func NewWithRecommendedAgentVersionBackend(backend database.Backend, recommendedAgentVersion string) *Service {
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

func (s *Service) IngestWire(ctx context.Context, expectedNodeID uuid.UUID, payload []byte) (bool, error) {
	batch, payloadBytes, err := s.validateWire(expectedNodeID, payload)
	if err != nil {
		return false, err
	}
	return s.ingest(ctx, batch, payloadBytes)
}

// IngestWireTx writes a validated telemetry batch using the caller's
// authenticated node identity and transaction so the authoritative endpoint
// check and every resulting state change share one commit boundary.
func (s *Service) IngestWireTx(ctx context.Context, tx pgx.Tx, expectedNodeID uuid.UUID, payload []byte) (bool, error) {
	batch, payloadBytes, err := s.validateWire(expectedNodeID, payload)
	if err != nil {
		return false, err
	}
	return s.ingestTx(ctx, postgres.WrapTx(tx), batch, payloadBytes)
}

func (s *Service) validateWire(expectedNodeID uuid.UUID, payload []byte) (Batch, int, error) {
	batch, err := decodeWire(payload)
	if err != nil {
		return Batch{}, 0, fmt.Errorf("%w: %v", ErrInvalidTelemetry, err)
	}
	if batch.NodeID != expectedNodeID {
		return Batch{}, 0, fmt.Errorf("%w: telemetry node identity mismatch", ErrInvalidTelemetry)
	}
	payloadBytes, err := s.validateForIngest(batch)
	if err != nil {
		return Batch{}, 0, fmt.Errorf("%w: %v", ErrInvalidTelemetry, err)
	}
	return batch, payloadBytes, nil
}

func decodeWire(payload []byte) (Batch, error) {
	if len(payload) == 0 || len(payload) > MaxBatchBytes {
		return Batch{}, errors.New("telemetry wire batch size invalid")
	}
	var wire agentv1.TelemetryBatch
	if err := proto.Unmarshal(payload, &wire); err != nil {
		return Batch{}, errors.New("telemetry wire protobuf invalid")
	}
	batchID, err := uuid.FromBytes(wire.GetBatchId())
	if err != nil {
		return Batch{}, errors.New("telemetry batch ID invalid")
	}
	nodeID, err := uuid.FromBytes(wire.GetNodeId())
	if err != nil {
		return Batch{}, errors.New("telemetry node ID invalid")
	}
	snapshot := wire.GetSnapshot()
	if snapshot == nil || snapshot.GetObservedAt() == nil || snapshot.GetObservedAt().CheckValid() != nil {
		return Batch{}, errors.New("telemetry observed timestamp invalid")
	}
	instance, err := uuid.FromBytes(snapshot.GetAgentInstanceId())
	if err != nil {
		return Batch{}, errors.New("telemetry agent instance invalid")
	}
	kinds := map[agentv1.TelemetryPriority]string{agentv1.TelemetryPriority_TELEMETRY_PRIORITY_SECURITY: "security", agentv1.TelemetryPriority_TELEMETRY_PRIORITY_CURRENT_HEALTH: "current_health", agentv1.TelemetryPriority_TELEMETRY_PRIORITY_AGGREGATE: "aggregate", agentv1.TelemetryPriority_TELEMETRY_PRIORITY_RAW_HISTORY: "raw_history"}
	kind := kinds[wire.GetPriority()]
	dropped := snapshot.GetDropped()
	batch := Batch{ID: batchID, NodeID: nodeID, Sequence: wire.GetSequence(), Kind: kind, Snapshot: Snapshot{ObservedAt: snapshot.GetObservedAt().AsTime(), BootID: snapshot.GetBootId(), AgentInstance: instance, AgentVersion: snapshot.GetAgentVersion(), OcservVersion: snapshot.GetOcservVersion(), OSRelease: snapshot.GetOsRelease(), Architecture: snapshot.GetArchitecture(), Ocserv: snapshot.GetOcservJson(), System: snapshot.GetSystemJson(), Path: snapshot.GetPathJson()}}
	if dropped != nil {
		batch.Snapshot.Dropped = DropCounters{Security: dropped.GetSecurity(), Health: dropped.GetHealth(), Aggregate: dropped.GetAggregate(), Raw: dropped.GetRaw()}
	}
	for _, item := range snapshot.GetUpgradeResults() {
		operationID, operationErr := uuid.FromBytes(item.GetOperationId())
		if operationErr != nil || item.GetCompletedUnixMs() == 0 || item.GetCompletedUnixMs() > math.MaxInt64 {
			return Batch{}, errors.New("upgrade result report identity or time invalid")
		}
		var proof *agentv1.AgentUpgradeResultProof
		if item.GetPrivilegedResultProof() != nil {
			proof = proto.Clone(item.GetPrivilegedResultProof()).(*agentv1.AgentUpgradeResultProof)
		}
		batch.Snapshot.UpgradeResults = append(batch.Snapshot.UpgradeResults, UpgradeResult{OperationID: operationID, State: agentUpgradeOutcomeState(item.GetState()), TargetVersion: item.GetTargetVersion(), CompletedAt: time.UnixMilli(int64(item.GetCompletedUnixMs())).UTC(), Detail: item.GetDetail(), Proof: proof})
	}
	for _, item := range wire.GetSessions() {
		if item.GetConnectedAt() == nil || item.GetConnectedAt().CheckValid() != nil {
			return Batch{}, errors.New("session timestamp invalid")
		}
		if item.GetBytesIn() > math.MaxInt64 || item.GetBytesOut() > math.MaxInt64 {
			return Batch{}, errors.New("session byte count invalid")
		}
		batch.Sessions = append(batch.Sessions, Session{ID: item.GetSessionId(), Username: item.GetUsername(), ClientIP: item.GetClientIp(), ConnectedAt: item.GetConnectedAt().AsTime(), BytesIn: int64(item.GetBytesIn()), BytesOut: int64(item.GetBytesOut())})
	}
	for _, item := range wire.GetIpBans() {
		batch.IPBans = append(batch.IPBans, IPBan{IP: item.GetIp(), SecondsRemaining: item.SecondsRemaining})
	}
	for _, item := range wire.GetUsers() {
		batch.Users = append(batch.Users, User{Username: item.GetUsername(), Enabled: item.GetEnabled(), Revision: item.GetRevision(), Fingerprint: item.GetFingerprintSha256()})
	}
	for _, item := range wire.GetGroups() {
		batch.Groups = append(batch.Groups, Group{Name: item.GetGroupName(), Members: item.GetMembers(), Revision: item.GetRevision(), Fingerprint: item.GetFingerprintSha256()})
	}
	for _, item := range wire.GetSamples() {
		if item.GetSampledAt() == nil || item.GetSampledAt().CheckValid() != nil {
			return Batch{}, errors.New("sample timestamp invalid")
		}
		batch.Samples = append(batch.Samples, Sample{SampledAt: item.GetSampledAt().AsTime(), Metric: item.GetMetric(), Value: item.GetValue()})
	}
	for _, item := range wire.GetSecurityEvents() {
		id, err := uuid.FromBytes(item.GetEventId())
		if err != nil {
			return Batch{}, errors.New("security event ID invalid")
		}
		if item.GetObservedAt() == nil || item.GetObservedAt().CheckValid() != nil {
			return Batch{}, errors.New("security event timestamp invalid")
		}
		batch.Security = append(batch.Security, SecurityEvent{ID: id, ObservedAt: item.GetObservedAt().AsTime(), Severity: item.GetSeverity(), Type: item.GetEventType(), Detail: item.GetDetailJson()})
	}
	return batch, nil
}

func (s *Service) Ingest(ctx context.Context, batch Batch) (bool, error) {
	payloadBytes, err := s.validateForIngest(batch)
	if err != nil {
		return false, err
	}
	return s.ingest(ctx, batch, payloadBytes)
}

func (s *Service) validateForIngest(batch Batch) (int, error) {
	payload, err := json.Marshal(batch)
	if err != nil || len(payload) > MaxBatchBytes {
		return 0, errors.New("telemetry batch exceeds 512 KiB or is invalid")
	}
	if err := validateBatch(batch, s.now()); err != nil {
		return 0, err
	}
	return len(payload), nil
}

func (s *Service) ingest(ctx context.Context, batch Batch, payloadBytes int) (bool, error) {
	var ingested bool
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		var err error
		ingested, err = s.ingestTx(ctx, tx, batch, payloadBytes)
		return err
	})
	return ingested && err == nil, err
}

// IngestWireTransaction keeps authentication and all ingestion writes in the caller's transaction.
func (s *Service) IngestWireTransaction(ctx context.Context, tx database.Tx, expectedNodeID uuid.UUID, payload []byte) (bool, error) {
	batch, size, err := s.validateWire(expectedNodeID, payload)
	if err != nil {
		return false, err
	}
	return s.ingestTx(ctx, tx, batch, size)
}
func (s *Service) ingestTx(ctx context.Context, tx database.Tx, batch Batch, payloadBytes int) (bool, error) {
	store, err := telemetrywrite.From(tx)
	if err != nil {
		return false, err
	}
	if err := store.LockNode(ctx, batch.NodeID); errors.Is(err, database.ErrNotFound) {
		return false, fmt.Errorf("%w: telemetry node is unavailable", ErrInvalidTelemetry)
	} else if err != nil {
		return false, fmt.Errorf("lock telemetry node: %w", err)
	}
	documents := []json.RawMessage{batch.Snapshot.Ocserv, batch.Snapshot.System, batch.Snapshot.Path}
	for _, e := range batch.Security {
		documents = append(documents, e.Detail)
	}
	if err := store.ValidateJSON(ctx, documents); err != nil {
		if errors.Is(err, telemetrywrite.ErrInvalidJSON) {
			return false, fmt.Errorf("%w: %v", ErrInvalidTelemetry, err)
		}
		return false, err
	}
	inserted, err := store.InsertBatch(ctx, batch.ID, batch.NodeID, batch.Sequence, batch.Kind, batch.Snapshot.ObservedAt, payloadBytes)
	if err != nil {
		return false, fmt.Errorf("insert telemetry batch: %w", err)
	}
	if !inserted {
		return false, nil
	}
	snap := batch.Snapshot
	updated, err := store.UpsertSnapshot(ctx, batch.NodeID, telemetrywrite.Snapshot{
		ObservedAt: snap.ObservedAt, BootID: snap.BootID, AgentInstance: snap.AgentInstance,
		AgentVersion: snap.AgentVersion, OcservVersion: snap.OcservVersion, OSRelease: snap.OSRelease, Architecture: snap.Architecture,
		Ocserv: snap.Ocserv, System: snap.System, Path: snap.Path, Security: snap.Dropped.Security, Health: snap.Dropped.Health, Aggregate: snap.Dropped.Aggregate, Raw: snap.Dropped.Raw,
	})
	if err != nil {
		return false, fmt.Errorf("upsert observed snapshot: %w", err)
	}
	if updated {
		usage := make([]userusage.Sample, 0, len(batch.Sessions))
		for _, session := range batch.Sessions {
			usage = append(usage, userusage.Sample{SessionID: session.ID, Username: session.Username, Connected: session.ConnectedAt, RXBytes: session.BytesIn, TXBytes: session.BytesOut, ObservedAt: batch.Snapshot.ObservedAt})
		}
		if err := userusage.RecordTransaction(ctx, tx, batch.NodeID, usage); err != nil {
			if errors.Is(err, userusage.ErrInvalidSample) {
				return false, fmt.Errorf("%w: session usage identity conflict", ErrInvalidTelemetry)
			}
			return false, fmt.Errorf("record observed user usage: %w", err)
		}

		sessions := make([]telemetrywrite.Session, 0, len(batch.Sessions))
		for _, v := range batch.Sessions {
			sessions = append(sessions, telemetrywrite.Session{ID: v.ID, Username: v.Username, ClientIP: v.ClientIP, ConnectedAt: v.ConnectedAt, BytesIn: v.BytesIn, BytesOut: v.BytesOut})
		}
		if err := store.ReplaceSessions(ctx, batch.NodeID, snap.ObservedAt, sessions); err != nil {
			return false, err
		}
		bans := make([]telemetrywrite.IPBan, 0, len(batch.IPBans))
		for _, v := range batch.IPBans {
			bans = append(bans, telemetrywrite.IPBan{IP: v.IP, SecondsRemaining: v.SecondsRemaining})
		}
		if err := store.ReplaceIPBans(ctx, batch.NodeID, snap.ObservedAt, bans); err != nil {
			return false, err
		}
		users := make([]telemetrywrite.User, 0, len(batch.Users))
		for _, v := range batch.Users {
			users = append(users, telemetrywrite.User{Username: v.Username, Enabled: v.Enabled, Revision: v.Revision, Fingerprint: v.Fingerprint})
		}
		if err := store.ReplaceUsers(ctx, batch.NodeID, snap.ObservedAt, users); err != nil {
			return false, err
		}
		if err := replaceObservedGroups(ctx, tx, batch); err != nil {
			return false, fmt.Errorf("replace observed groups: %w", err)
		}

		if snap.ObservedAt.After(s.now().Add(-OfflineAfter)) {
			if err := store.Activate(ctx, batch.NodeID, snap.ObservedAt); err != nil {
				return false, err
			}
		}
	}
	for _, report := range snap.UpgradeResults {
		verification, err := privdattestation.VerifyUpgradeResultTransaction(ctx, tx, batch.NodeID, report.OperationID, agentUpgradeOutcomeProtoState(report.State), report.TargetVersion, report.CompletedAt, report.Proof)
		if err != nil {
			return false, fmt.Errorf("verify reported agent upgrade result: %w", err)
		}
		if !verification.Verified() {
			continue
		}
		if err := store.InsertUpgrade(ctx, batch.NodeID, telemetrywrite.Upgrade{OperationID: report.OperationID, State: report.State, TargetVersion: report.TargetVersion, Detail: report.Detail, CompletedAt: verification.CompletedAt, Proof: verification.EncodedProof, PackageSHA256: verification.PackageSHA256}); err != nil {
			return false, err
		}
	}
	if err := insertObservedSecurity(ctx, tx, batch); err != nil {
		return false, fmt.Errorf("insert security event: %w", err)
	}
	history, err := telemetryhistory.FromTransaction(tx)
	if err != nil {
		return false, err
	}
	samples := make([]telemetryhistory.Sample, 0, len(batch.Samples))
	for _, sample := range batch.Samples {
		samples = append(samples, telemetryhistory.Sample{SampledAt: sample.SampledAt, Metric: sample.Metric, Value: sample.Value})
	}
	if err := history.Insert(ctx, batch.NodeID, batch.ID, samples); err != nil {
		return false, err
	}
	return true, nil
}

func validateBatch(batch Batch, now time.Time) error {
	if batch.ID == uuid.Nil || batch.ID.Version() != 7 || batch.NodeID == uuid.Nil || batch.NodeID.Version() != 7 {
		return errors.New("telemetry IDs must be UUIDv7")
	}
	if batch.Sequence > math.MaxInt64 {
		return errors.New("telemetry sequence exceeds int64")
	}
	if batch.Kind != "security" && batch.Kind != "current_health" && batch.Kind != "aggregate" && batch.Kind != "raw_history" {
		return errors.New("telemetry kind is invalid")
	}
	earliest := now.Add(-MaxTelemetryAge)
	latest := now.Add(MaxTelemetrySkew)
	if batch.Snapshot.ObservedAt.IsZero() || batch.Snapshot.ObservedAt.Before(earliest) || batch.Snapshot.ObservedAt.After(latest) || !postgresinput.ValidText(batch.Snapshot.BootID, 128) || batch.Snapshot.AgentInstance == uuid.Nil {
		return errors.New("observed snapshot identity or time is invalid")
	}
	for _, value := range []string{batch.Snapshot.AgentVersion, batch.Snapshot.OcservVersion, batch.Snapshot.OSRelease} {
		if !postgresinput.ValidText(value, 128) {
			return errors.New("observed version is invalid")
		}
	}
	if batch.Snapshot.Architecture != "" && !semanticpayload.ValidAgentUpgradeArchitecture(batch.Snapshot.Architecture) {
		return errors.New("observed architecture is invalid")
	}
	if len(batch.Snapshot.UpgradeResults) > 8 {
		return errors.New("upgrade result report count exceeds limit")
	}
	seenUpgradeOperations := make(map[uuid.UUID]struct{}, len(batch.Snapshot.UpgradeResults))
	for _, report := range batch.Snapshot.UpgradeResults {
		if report.OperationID.Version() != 7 || report.State == "" || !semanticpayload.ValidAgentUpgradeTargetVersion(report.TargetVersion) || !postgresinput.ValidText(report.Detail, 160) || report.Proof == nil {
			return errors.New("upgrade result report is invalid")
		}
		if _, err := privdattestation.CanonicalAgentUpgradeResultProofV1(report.Proof); err != nil ||
			!bytes.Equal(report.Proof.GetNodeId(), batch.NodeID[:]) ||
			!bytes.Equal(report.Proof.GetOperationId(), report.OperationID[:]) ||
			report.Proof.GetState() != agentUpgradeOutcomeProtoState(report.State) ||
			report.Proof.GetTargetVersion() != report.TargetVersion ||
			!upgradeResultCompletedAtMatches(report.Proof, report.CompletedAt) {
			return errors.New("upgrade result proof claims are invalid")
		}
		if _, duplicate := seenUpgradeOperations[report.OperationID]; duplicate {
			return errors.New("duplicate upgrade result report")
		}
		seenUpgradeOperations[report.OperationID] = struct{}{}
	}
	for _, document := range []json.RawMessage{batch.Snapshot.Ocserv, batch.Snapshot.System, batch.Snapshot.Path} {
		if !validObject(document) {
			return errors.New("observed documents must be JSON objects")
		}
	}
	for _, counter := range []uint64{
		batch.Snapshot.Dropped.Security,
		batch.Snapshot.Dropped.Health,
		batch.Snapshot.Dropped.Aggregate,
		batch.Snapshot.Dropped.Raw,
	} {
		if counter > math.MaxInt64 {
			return errors.New("telemetry drop counter exceeds int64")
		}
	}
	if len(batch.Sessions) > 10000 || len(batch.IPBans) > 4096 || len(batch.Samples) > 8192 || len(batch.Security) > 1024 || len(batch.Users) > MaxManagedResources || len(batch.Groups) > MaxReportedGroups {
		return errors.New("telemetry collection count exceeds limit")
	}
	namePattern := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	usernames := make(map[string]struct{}, len(batch.Users))
	for _, user := range batch.Users {
		if !namePattern.MatchString(user.Username) || user.Revision > math.MaxInt64 || len(user.Fingerprint) != sha256.Size {
			return errors.New("user observation is invalid")
		}
		if !insertUnique(usernames, user.Username) {
			return errors.New("duplicate user observation")
		}
	}
	totalMemberships := 0
	groupNames := make(map[string]struct{}, len(batch.Groups))
	for _, group := range batch.Groups {
		totalMemberships += len(group.Members)
		if !namePattern.MatchString(group.Name) || group.Revision > math.MaxInt64 || len(group.Fingerprint) != sha256.Size || len(group.Members) > MaxManagedResources || totalMemberships > MaxManagedResources {
			return errors.New("group observation is invalid")
		}
		if !insertUnique(groupNames, group.Name) {
			return errors.New("duplicate group observation")
		}
		copyMembers := slices.Clone(group.Members)
		slices.Sort(copyMembers)
		if !slices.Equal(copyMembers, slices.Compact(copyMembers)) || !slices.Equal(copyMembers, group.Members) {
			return errors.New("group members must be sorted and unique")
		}
		for _, member := range group.Members {
			if !namePattern.MatchString(member) {
				return errors.New("group member observation is invalid")
			}
		}
	}
	banIPs := make(map[string]struct{}, len(batch.IPBans))
	for _, ban := range batch.IPBans {
		if ban.SecondsRemaining != nil && *ban.SecondsRemaining > math.MaxInt64 {
			return errors.New("IP ban remaining duration exceeds int64")
		}
		parsed := net.ParseIP(ban.IP)
		if parsed == nil || parsed.String() != ban.IP {
			return errors.New("IP ban observation is invalid")
		}
		if !insertUnique(banIPs, ban.IP) {
			return errors.New("duplicate IP ban observation")
		}
	}
	sessionIDs := make(map[string]struct{}, len(batch.Sessions))
	for _, session := range batch.Sessions {
		if !postgresinput.ValidText(session.ID, 256) || !namePattern.MatchString(session.Username) || net.ParseIP(session.ClientIP) == nil || session.BytesIn < 0 || session.BytesOut < 0 || session.ConnectedAt.IsZero() {
			return errors.New("session observation is invalid")
		}
		if !insertUnique(sessionIDs, session.ID) {
			return errors.New("duplicate session observation")
		}
	}
	for _, sample := range batch.Samples {
		if !allowedMetrics[sample.Metric] || math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) || sample.SampledAt.IsZero() || sample.SampledAt.Before(earliest) || sample.SampledAt.After(latest) {
			return errors.New("telemetry sample is invalid")
		}
	}
	for _, event := range batch.Security {
		if event.ID == uuid.Nil || event.ID.Version() != 7 || event.ObservedAt.IsZero() || event.ObservedAt.Before(earliest) || event.ObservedAt.After(latest) || (event.Severity != "info" && event.Severity != "warning" && event.Severity != "critical") || !postgresinput.ValidText(event.Type, 128) || !validObject(event.Detail) {
			return errors.New("security telemetry is invalid")
		}
	}
	return nil
}

func insertUnique[T comparable](values map[T]struct{}, value T) bool {
	if _, exists := values[value]; exists {
		return false
	}
	values[value] = struct{}{}
	return true
}

func validObject(value json.RawMessage) bool {
	if len(value) == 0 || !utf8.Valid(value) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return false
	}
	return decoder.Decode(&struct{}{}) == io.EOF
}

func (s *Service) ListNodes(ctx context.Context, after uuid.UUID, limit int) ([]Node, bool, error) {
	return s.ListNodesInWorkspace(ctx, uuid.Nil, after, limit)
}

func (s *Service) ListNodesInWorkspace(ctx context.Context, workspaceID, after uuid.UUID, limit int) ([]Node, bool, error) {
	if limit < 1 || limit > 200 {
		return nil, false, errors.New("node page size must be between 1 and 200")
	}
	var stored []telemetryread.Node
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := telemetryread.From(tx)
		if err != nil {
			return err
		}
		stored, err = store.Nodes(ctx, workspaceID, after, limit+1)
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("list nodes: %w", err)
	}
	result := []Node{}
	for _, v := range stored {
		node := readNode(v, s.now())
		s.applyAgentVersionState(&node)
		result = append(result, node)
	}
	hasMore := len(result) > limit
	if hasMore {
		result = result[:limit]
	}
	return result, hasMore, nil
}

func (s *Service) GetNode(ctx context.Context, id uuid.UUID) (Node, error) {
	var stored telemetryread.Node
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := telemetryread.From(tx)
		if err != nil {
			return err
		}
		stored, err = store.Node(ctx, id)
		return err
	})
	if err != nil {
		return Node{}, err
	}
	node := readNode(stored, s.now())
	s.applyAgentVersionState(&node)
	s.applyAgentUpgradeEligibility(ctx, &node)
	return node, nil
}

// applyAgentUpgradeEligibility derives the single-node upgrade workflow gate
// from durable state: an upgrade_available version state, an online fresh
// node with the approved upgrade capability, a trusted release for its
// architecture, and no conflicting active upgrade. It is never persisted and
// fails closed on any lookup error.
func (s *Service) applyAgentUpgradeEligibility(ctx context.Context, node *Node) {
	if s.agentUpgradeCatalog == nil || node.AgentVersionState != "upgrade_available" || node.ConnectionState != "online" || node.Freshness != "fresh" || node.Architecture == "" {
		return
	}
	if _, trusted := s.agentUpgradeCatalog.Lookup(node.RecommendedAgentVersion, node.Architecture); !trusted {
		return
	}
	nodeID, err := uuid.Parse(node.ID)
	if err != nil {
		return
	}
	// Only nodes that advertise the fence-capable v2 capability are eligible:
	// a v1 source runner would execute the first hop without the
	// execution-time downgrade fence and installation commit record.
	var eligible bool
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := telemetryread.From(tx)
		if err != nil {
			return err
		}
		eligible, err = store.UpgradeEligibility(ctx, nodeID)
		return err
	})
	if err != nil {
		return
	}
	node.AgentUpgradeEligible = eligible
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

func readNode(v telemetryread.Node, now time.Time) Node {
	n := Node{ID: v.ID.String(), Name: v.Name, Version: v.Version, TrustStatus: v.Status,
		BootID: v.BootID, AgentInstanceID: v.InstanceID, AgentVersion: v.AgentVersion,
		OcservVersion: v.OcservVersion, OSRelease: v.OSRelease, Architecture: v.Architecture,
		Ocserv: v.Ocserv.Bytes(), System: v.System.Bytes(), Path: v.Path.Bytes(), SessionCount: v.Sessions,
		Dropped: DropCounters{Security: v.Security, Health: v.Health, Aggregate: v.Aggregate, Raw: v.Raw}}
	if v.ObservedAt.Valid {
		n.ObservedAt = &v.ObservedAt
	}
	if v.Heartbeat.Valid {
		n.LastHeartbeatAt = &v.Heartbeat
	}
	if !v.Heartbeat.Valid {
		n.Freshness = "never"
		n.ConnectionState = "offline"
	} else {
		cutoff, err := value.FromTime(now.Add(-OfflineAfter))
		if err == nil && now.Nanosecond()%1000 != 0 {
			cutoff.Micros++
		}
		if err == nil && v.Heartbeat.Micros >= cutoff.Micros {
			n.Freshness = "fresh"
			n.ConnectionState = "online"
		} else {
			n.Freshness = "stale"
			n.ConnectionState = "offline"
		}
	}
	return n
}

func (s *Service) ListSessions(ctx context.Context, nodeID uuid.UUID, after string, limit int) ([]telemetryread.Session, bool, error) {
	if limit < 1 || limit > 200 || len(after) > 256 {
		return nil, false, errors.New("session page is invalid")
	}
	var result []telemetryread.Session
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := telemetryread.From(tx)
		if err != nil {
			return err
		}
		result, err = store.Sessions(ctx, nodeID, after, limit+1)
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
	if since.IsZero() {
		since = s.now().Add(-24 * time.Hour)
	}
	at, err := value.FromTime(since)
	if err != nil {
		return nil, err
	}
	return s.HistoryFrom(ctx, nodeID, metric, resolution, at)
}

// HistoryFrom retains infinite and extended finite query bounds and results.
// A NULL bound means the same default window as an omitted HTTP query.
func (s *Service) HistoryFrom(ctx context.Context, nodeID uuid.UUID, metric, resolution string, since value.Timestamp) ([]HistoryPoint, error) {
	if !allowedMetrics[metric] {
		return nil, ErrInvalidMetric
	}
	if !since.Valid {
		var err error
		since, err = value.FromTime(s.now().Add(-24 * time.Hour))
		if err != nil {
			return nil, err
		}
	}
	if resolution != "raw" && resolution != "5m" && resolution != "1h" {
		return nil, ErrInvalidResolution
	}
	var points []HistoryPoint
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		history, err := telemetryhistory.FromTransaction(tx)
		if err != nil {
			return err
		}
		points, err = history.History(ctx, nodeID, metric, resolution, since)
		return err
	})
	return points, err
}

func (s *Service) Maintain(ctx context.Context) error {
	now := s.now().UTC()
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := telemetrywrite.From(tx)
		if err != nil {
			return err
		}
		offline, err := store.LockOfflineCandidates(ctx, now.Add(-OfflineAfter))
		if err != nil {
			return err
		}
		for _, id := range offline {
			if err := store.MarkOffline(ctx, id, uuid.Must(uuid.NewV7()), now, newTraceparent()); err != nil {
				return err
			}
		}
		history, err := telemetryhistory.FromTransaction(tx)
		if err != nil {
			return err
		}
		if err := history.Maintain(ctx, now); err != nil {
			return err
		}
		return coordination.AssertFenceTx(ctx, tx, coordination.FenceFromContext(ctx))
	})
}

func newTraceparent() string {
	trace := uuid.Must(uuid.NewV7())
	span := uuid.Must(uuid.NewV7())
	return "00-" + hex.EncodeToString(trace[:]) + "-" + hex.EncodeToString(span[:8]) + "-01"
}
