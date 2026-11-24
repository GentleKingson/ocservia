package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func (s *Service) IngestWire(ctx context.Context, expectedNodeID uuid.UUID, payload []byte) (bool, error) {
	batch, payloadBytes, err := s.validateWire(expectedNodeID, payload)
	if err != nil {
		return false, err
	}
	return s.ingest(ctx, batch, payloadBytes)
}

func (s *Service) validateWire(expectedNodeID uuid.UUID, payload []byte) (Batch, int, error) {
	batch, err := decodeWire(payload)
	if err != nil {
		return Batch{}, 0, fmt.Errorf("%w: %v", ErrInvalidTelemetry, err)
	}
	if batch.NodeID != expectedNodeID {
		return Batch{}, 0, fmt.Errorf("%w: telemetry node identity mismatch", ErrInvalidTelemetry)
	}
	// decodeWire already enforces 0 < len(payload) <= MaxBatchBytes on the wire
	// frame; skip throwaway JSON re-serialization of the decoded Batch.
	if err := validateBatch(batch, s.now()); err != nil {
		return Batch{}, 0, fmt.Errorf("%w: %v", ErrInvalidTelemetry, err)
	}
	return batch, len(payload), nil
}

func wirePriorityKind(priority agentv1.TelemetryPriority) string {
	switch priority {
	case agentv1.TelemetryPriority_TELEMETRY_PRIORITY_SECURITY:
		return "security"
	case agentv1.TelemetryPriority_TELEMETRY_PRIORITY_CURRENT_HEALTH:
		return "current_health"
	case agentv1.TelemetryPriority_TELEMETRY_PRIORITY_AGGREGATE:
		return "aggregate"
	case agentv1.TelemetryPriority_TELEMETRY_PRIORITY_RAW_HISTORY:
		return "raw_history"
	default:
		return ""
	}
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
	dropped := snapshot.GetDropped()
	batch := Batch{
		ID:       batchID,
		NodeID:   nodeID,
		Sequence: wire.GetSequence(),
		Kind:     wirePriorityKind(wire.GetPriority()),
		Snapshot: Snapshot{
			ObservedAt:    snapshot.GetObservedAt().AsTime(),
			BootID:        snapshot.GetBootId(),
			AgentInstance: instance,
			AgentVersion:  snapshot.GetAgentVersion(),
			OcservVersion: snapshot.GetOcservVersion(),
			OSRelease:     snapshot.GetOsRelease(),
			Architecture:  snapshot.GetArchitecture(),
			Ocserv:        snapshot.GetOcservJson(),
			System:        snapshot.GetSystemJson(),
			Path:          snapshot.GetPathJson(),
		},
	}
	if dropped != nil {
		batch.Snapshot.Dropped = DropCounters{Security: dropped.GetSecurity(), Health: dropped.GetHealth(), Aggregate: dropped.GetAggregate(), Raw: dropped.GetRaw()}
	}
	if rawUpgrades := snapshot.GetUpgradeResults(); len(rawUpgrades) > 0 {
		batch.Snapshot.UpgradeResults = make([]UpgradeResult, 0, len(rawUpgrades))
		for _, item := range rawUpgrades {
			operationID, operationErr := uuid.FromBytes(item.GetOperationId())
			if operationErr != nil || item.GetCompletedUnixMs() == 0 || item.GetCompletedUnixMs() > math.MaxInt64 {
				return Batch{}, errors.New("upgrade result report identity or time invalid")
			}
			var proof *agentv1.AgentUpgradeResultProof
			if item.GetPrivilegedResultProof() != nil {
				proof = proto.Clone(item.GetPrivilegedResultProof()).(*agentv1.AgentUpgradeResultProof)
			}
			batch.Snapshot.UpgradeResults = append(batch.Snapshot.UpgradeResults, UpgradeResult{
				OperationID:   operationID,
				State:         agentUpgradeOutcomeState(item.GetState()),
				TargetVersion: item.GetTargetVersion(),
				CompletedAt:   time.UnixMilli(int64(item.GetCompletedUnixMs())).UTC(),
				Detail:        item.GetDetail(),
				Proof:         proof,
			})
		}
	}
	if rawSessions := wire.GetSessions(); len(rawSessions) > 0 {
		batch.Sessions = make([]Session, 0, len(rawSessions))
		for _, item := range rawSessions {
			if item.GetConnectedAt() == nil || item.GetConnectedAt().CheckValid() != nil {
				return Batch{}, errors.New("session timestamp invalid")
			}
			if item.GetBytesIn() > math.MaxInt64 || item.GetBytesOut() > math.MaxInt64 {
				return Batch{}, errors.New("session byte count invalid")
			}
			batch.Sessions = append(batch.Sessions, Session{
				ID:          item.GetSessionId(),
				Username:    item.GetUsername(),
				ClientIP:    item.GetClientIp(),
				VPNIP:       item.GetVpnIp(),
				ConnectedAt: item.GetConnectedAt().AsTime(),
				BytesIn:     int64(item.GetBytesIn()),
				BytesOut:    int64(item.GetBytesOut()),
			})
		}
	}
	if rawBans := wire.GetIpBans(); len(rawBans) > 0 {
		batch.IPBans = make([]IPBan, 0, len(rawBans))
		for _, item := range rawBans {
			batch.IPBans = append(batch.IPBans, IPBan{IP: item.GetIp(), SecondsRemaining: item.SecondsRemaining})
		}
	}
	if rawUsers := wire.GetUsers(); len(rawUsers) > 0 {
		batch.Users = make([]User, 0, len(rawUsers))
		for _, item := range rawUsers {
			batch.Users = append(batch.Users, User{Username: item.GetUsername(), Enabled: item.GetEnabled(), Revision: item.GetRevision()})
		}
	}
	if rawGroups := wire.GetGroups(); len(rawGroups) > 0 {
		batch.Groups = make([]Group, 0, len(rawGroups))
		for _, item := range rawGroups {
			batch.Groups = append(batch.Groups, Group{Name: item.GetGroupName(), Members: item.GetMembers(), Revision: item.GetRevision()})
		}
	}
	if rawSamples := wire.GetSamples(); len(rawSamples) > 0 {
		batch.Samples = make([]Sample, 0, len(rawSamples))
		for _, item := range rawSamples {
			if item.GetSampledAt() == nil || item.GetSampledAt().CheckValid() != nil {
				return Batch{}, errors.New("sample timestamp invalid")
			}
			batch.Samples = append(batch.Samples, Sample{SampledAt: item.GetSampledAt().AsTime(), Metric: item.GetMetric(), Value: item.GetValue()})
		}
	}
	if rawSecurity := wire.GetSecurityEvents(); len(rawSecurity) > 0 {
		batch.Security = make([]SecurityEvent, 0, len(rawSecurity))
		for _, item := range rawSecurity {
			id, err := uuid.FromBytes(item.GetEventId())
			if err != nil {
				return Batch{}, errors.New("security event ID invalid")
			}
			if item.GetObservedAt() == nil || item.GetObservedAt().CheckValid() != nil {
				return Batch{}, errors.New("security event timestamp invalid")
			}
			batch.Security = append(batch.Security, SecurityEvent{ID: id, ObservedAt: item.GetObservedAt().AsTime(), Severity: item.GetSeverity(), Type: item.GetEventType(), Detail: item.GetDetailJson()})
		}
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
	store, err := telemetrystore.FromTransaction(tx)
	if err != nil {
		return false, err
	}
	if err := store.LockNode(ctx, batch.NodeID); errors.Is(err, database.ErrNotFound) {
		return false, fmt.Errorf("%w: telemetry node is unavailable", ErrInvalidTelemetry)
	} else if err != nil {
		return false, fmt.Errorf("lock telemetry node: %w", err)
	}
	events := make([]telemetrystore.SecurityEvent, 0, len(batch.Security))
	for _, e := range batch.Security {
		events = append(events, telemetrystore.SecurityEvent{ID: e.ID, ObservedAt: e.ObservedAt.UTC(), Severity: e.Severity, Type: e.Type, Detail: e.Detail})
	}
	snap := batch.Snapshot
	inserted, updated, err := store.InsertObservation(ctx, batch.ID, batch.NodeID, batch.Sequence, batch.Kind, payloadBytes, telemetrystore.Snapshot{
		ObservedAt: snap.ObservedAt, BootID: snap.BootID, AgentInstance: snap.AgentInstance,
		AgentVersion: snap.AgentVersion, OcservVersion: snap.OcservVersion, OSRelease: snap.OSRelease, Architecture: snap.Architecture,
		Ocserv: snap.Ocserv, System: snap.System, Path: snap.Path, Security: snap.Dropped.Security, Health: snap.Dropped.Health, Aggregate: snap.Dropped.Aggregate, Raw: snap.Dropped.Raw,
	}, events)
	if errors.Is(err, telemetrystore.ErrInvalidJSON) {
		return false, fmt.Errorf("%w: %v", ErrInvalidTelemetry, err)
	}
	if err != nil {
		return false, fmt.Errorf("insert telemetry observation: %w", err)
	}
	if !inserted {
		return false, nil
	}
	if updated {
		usage := make([]userstore.UsageSample, 0, len(batch.Sessions))
		for _, session := range batch.Sessions {
			usage = append(usage, userstore.UsageSample{SessionID: session.ID, Username: session.Username, Connected: session.ConnectedAt, RXBytes: session.BytesIn, TXBytes: session.BytesOut, ObservedAt: batch.Snapshot.ObservedAt})
		}
		if err := userstore.RecordUsageTransaction(ctx, tx, batch.NodeID, usage); err != nil {
			if errors.Is(err, userstore.ErrInvalidUsageSample) {
				return false, fmt.Errorf("%w: session usage identity conflict", ErrInvalidTelemetry)
			}
			return false, fmt.Errorf("record observed user usage: %w", err)
		}

		sessions := make([]telemetrystore.Session, 0, len(batch.Sessions))
		for _, v := range batch.Sessions {
			sessions = append(sessions, telemetrystore.Session{ID: v.ID, Username: v.Username, ClientIP: v.ClientIP, VPNIP: v.VPNIP, ConnectedAt: v.ConnectedAt, BytesIn: v.BytesIn, BytesOut: v.BytesOut})
		}
		if err := store.ReplaceSessions(ctx, batch.NodeID, snap.ObservedAt, sessions); err != nil {
			return false, err
		}
		bans := make([]telemetrystore.IPBan, 0, len(batch.IPBans))
		for _, v := range batch.IPBans {
			bans = append(bans, telemetrystore.IPBan{IP: v.IP, SecondsRemaining: v.SecondsRemaining})
		}
		if err := store.ReplaceIPBans(ctx, batch.NodeID, snap.ObservedAt, bans); err != nil {
			return false, err
		}
		users := make([]telemetrystore.User, 0, len(batch.Users))
		for _, v := range batch.Users {
			users = append(users, telemetrystore.User{Username: v.Username, Enabled: v.Enabled, Revision: v.Revision})
		}
		if err := store.ReplaceUsers(ctx, batch.NodeID, snap.ObservedAt, users); err != nil {
			return false, err
		}
		if err := replaceObservedGroups(ctx, store, batch); err != nil {
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
		if err := store.InsertUpgrade(ctx, batch.NodeID, telemetrystore.Upgrade{OperationID: report.OperationID, State: report.State, TargetVersion: report.TargetVersion, Detail: report.Detail, CompletedAt: verification.CompletedAt, Proof: verification.EncodedProof, PackageSHA256: verification.PackageSHA256}); err != nil {
			return false, err
		}
	}
	samples := make([]telemetrystore.Sample, 0, len(batch.Samples))
	for _, sample := range batch.Samples {
		samples = append(samples, telemetrystore.Sample{SampledAt: sample.SampledAt, Metric: sample.Metric, Value: sample.Value})
	}
	if err := store.InsertHistory(ctx, batch.NodeID, batch.ID, samples); err != nil {
		return false, err
	}
	return true, nil
}

// validObservedName matches `^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$` without regex compilation.
func validObservedName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	first := s[0]
	if !((first >= 'A' && first <= 'Z') || (first >= 'a' && first <= 'z') || (first >= '0' && first <= '9')) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
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
	if batch.Snapshot.ObservedAt.IsZero() || batch.Snapshot.ObservedAt.Before(earliest) || batch.Snapshot.ObservedAt.After(latest) || !validText(batch.Snapshot.BootID, 128) || batch.Snapshot.AgentInstance == uuid.Nil {
		return errors.New("observed snapshot identity or time is invalid")
	}
	for _, text := range [3]string{batch.Snapshot.AgentVersion, batch.Snapshot.OcservVersion, batch.Snapshot.OSRelease} {
		if !validText(text, 128) {
			return errors.New("observed version is invalid")
		}
	}
	if batch.Snapshot.Architecture != "" && !semanticpayload.ValidAgentUpgradeArchitecture(batch.Snapshot.Architecture) {
		return errors.New("observed architecture is invalid")
	}
	if len(batch.Snapshot.UpgradeResults) > 8 {
		return errors.New("upgrade result report count exceeds limit")
	}
	if len(batch.Snapshot.UpgradeResults) > 0 {
		seenUpgradeOperations := make(map[uuid.UUID]struct{}, len(batch.Snapshot.UpgradeResults))
		for _, report := range batch.Snapshot.UpgradeResults {
			if report.OperationID.Version() != 7 || report.State == "" || !semanticpayload.ValidAgentUpgradeTargetVersion(report.TargetVersion) || !validText(report.Detail, 160) || report.Proof == nil {
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
	}
	if !validObject(batch.Snapshot.Ocserv) || !validObject(batch.Snapshot.System) || !validObject(batch.Snapshot.Path) {
		return errors.New("observed documents must be JSON objects")
	}
	if batch.Snapshot.Dropped.Security > math.MaxInt64 ||
		batch.Snapshot.Dropped.Health > math.MaxInt64 ||
		batch.Snapshot.Dropped.Aggregate > math.MaxInt64 ||
		batch.Snapshot.Dropped.Raw > math.MaxInt64 {
		return errors.New("telemetry drop counter exceeds int64")
	}
	if len(batch.Sessions) > 10000 || len(batch.IPBans) > 4096 || len(batch.Samples) > 8192 || len(batch.Security) > 1024 || len(batch.Users) > MaxManagedResources || len(batch.Groups) > MaxReportedGroups {
		return errors.New("telemetry collection count exceeds limit")
	}
	if len(batch.Users) > 0 {
		usernames := make(map[string]struct{}, len(batch.Users))
		for _, user := range batch.Users {
			if !validObservedName(user.Username) || user.Revision > math.MaxInt64 {
				return errors.New("user observation is invalid")
			}
			if !insertUnique(usernames, user.Username) {
				return errors.New("duplicate user observation")
			}
		}
	}
	if len(batch.Groups) > 0 {
		totalMemberships := 0
		groupNames := make(map[string]struct{}, len(batch.Groups))
		for _, group := range batch.Groups {
			totalMemberships += len(group.Members)
			if !validObservedName(group.Name) || group.Revision > math.MaxInt64 || len(group.Members) > MaxManagedResources || totalMemberships > MaxManagedResources {
				return errors.New("group observation is invalid")
			}
			if !insertUnique(groupNames, group.Name) {
				return errors.New("duplicate group observation")
			}
			for i, member := range group.Members {
				if i > 0 && group.Members[i-1] >= member {
					return errors.New("group members must be sorted and unique")
				}
				if !validObservedName(member) {
					return errors.New("group member observation is invalid")
				}
			}
		}
	}
	if len(batch.IPBans) > 0 {
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
	}
	if len(batch.Sessions) > 0 {
		sessionIDs := make(map[string]struct{}, len(batch.Sessions))
		for _, session := range batch.Sessions {
			if !validText(session.ID, 256) || !validObservedName(session.Username) || net.ParseIP(session.ClientIP) == nil || (session.VPNIP != "" && net.ParseIP(session.VPNIP) == nil) || session.BytesIn < 0 || session.BytesOut < 0 || session.ConnectedAt.IsZero() {
				return errors.New("session observation is invalid")
			}
			if !insertUnique(sessionIDs, session.ID) {
				return errors.New("duplicate session observation")
			}
		}
	}
	for _, sample := range batch.Samples {
		if !allowedMetrics[sample.Metric] || math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) || sample.SampledAt.IsZero() || sample.SampledAt.Before(earliest) || sample.SampledAt.After(latest) {
			return errors.New("telemetry sample is invalid")
		}
	}
	for _, event := range batch.Security {
		if event.ID == uuid.Nil || event.ID.Version() != 7 || event.ObservedAt.IsZero() || event.ObservedAt.Before(earliest) || event.ObservedAt.After(latest) || (event.Severity != "info" && event.Severity != "warning" && event.Severity != "critical") || !validText(event.Type, 128) || !validObject(event.Detail) {
			return errors.New("security telemetry is invalid")
		}
	}
	return nil
}

func validText(s string, maxBytes int) bool {
	return s != "" && len(s) <= maxBytes && utf8.ValidString(s) && strings.IndexByte(s, 0) < 0
}

func insertUnique[T comparable](values map[T]struct{}, v T) bool {
	if _, exists := values[v]; exists {
		return false
	}
	values[v] = struct{}{}
	return true
}

func validObject(raw json.RawMessage) bool {
	if len(raw) == 0 || !utf8.Valid(raw) {
		return false
	}
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	return json.Valid(trimmed)
}
