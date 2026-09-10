package localslice

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	localstore "github.com/GentleKingson/ocservia/control-plane/internal/localslice/store"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const workspaceSlug = "local-simulator"

var ErrInvalidScenario = errors.New("invalid simulation")

type Scenario struct {
	HeartbeatCount  *uint32 `json:"heartbeat_count"`
	DelayMillis     *uint32 `json:"delay_millis"`
	DuplicateEvent  bool    `json:"duplicate_event"`
	ReturnError     bool    `json:"return_error"`
	DisconnectAfter bool    `json:"disconnect_after"`
}

type Operation struct {
	ID        string          `json:"id"`
	State     string          `json:"state"`
	NodeID    *string         `json:"node_id,omitempty"`
	CommandID *string         `json:"command_id,omitempty"`
	Version   int64           `json:"version"`
	CreatedAt value.Timestamp `json:"created_at"`
	UpdatedAt value.Timestamp `json:"updated_at"`
}

type Event = localstore.Event
type Job = localstore.Job

type Service struct {
	backend                database.Backend
	now                    func() time.Time
	signer                 *commandauth.Signer
	commandRecovery        CommandRecoverer
	recoveryAuthority      RecoveryAuthority
	resultCommitBarrierDir string
}

// CommandRecoverer is the narrow business-layer hook used after transportd
// reports that a fenced Agent connection is registered and usable. The
// implementation must recheck database ownership inside the supplied event
// transaction before it changes command or outbox state.
type CommandRecoverer interface {
	RecoverAmbiguousDispatched(context.Context, database.Tx, operationstore.OwnerReconnect) (int, error)
}

// RecoveryAuthority binds reconnect scheduling to the process that owns the
// exact local session term. Database ownership is rechecked by CommandRecoverer
// inside the state-changing transaction.
type RecoveryAuthority interface {
	OwnsTerm(nodeID, connectionID [16]byte, ownerEpoch int64) bool
}

const (
	maxTransportEventAge = telemetrystore.MaxTelemetryAge
	maxQuarantineDetail  = 256
)

type permanentInvalidEvent struct {
	reasonCode string
	detail     string
}

func (e *permanentInvalidEvent) Error() string { return e.detail }

func invalidEvent(reasonCode, detail string) error {
	if len(detail) > maxQuarantineDetail {
		detail = detail[:maxQuarantineDetail]
	}
	return &permanentInvalidEvent{reasonCode: reasonCode, detail: detail}
}

func New(pool *pgxpool.Pool) *Service {
	return NewBackend(postgres.WrapPool(pool), nil)
}

func NewBackend(backend database.Backend, signer *commandauth.Signer) *Service {
	return &Service{backend: backend, signer: signer, now: func() time.Time { return time.Now().UTC() }}
}

func NewBackendWithCommandRecovery(backend database.Backend, signer *commandauth.Signer, recovery CommandRecoverer, authority RecoveryAuthority) *Service {
	service := NewBackend(backend, signer)
	service.commandRecovery, service.recoveryAuthority = recovery, authority
	return service
}

// NewWithSigner configures recovery redispatches with a Controller signer.
func NewWithSigner(pool *pgxpool.Pool, signer *commandauth.Signer) *Service {
	service := New(pool)
	service.signer = signer
	return service
}

// NewWithCommandRecovery enables authoritative reconnect reconciliation while
// leaving ownership acquisition and renewal inside ownersession.
func NewWithCommandRecovery(pool *pgxpool.Pool, signer *commandauth.Signer, recovery CommandRecoverer, authority RecoveryAuthority) *Service {
	service := NewWithSigner(pool, signer)
	service.commandRecovery, service.recoveryAuthority = recovery, authority
	return service
}

// EnableResultCommitBarrier configures the development-harness barrier used
// to stop a validated command result inside its still-open database
// transaction. Production configuration rejects this hook before startup.
func (s *Service) EnableResultCommitBarrier(directory string) error {
	info, err := os.Stat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("result commit barrier path is not a directory")
	}
	s.resultCommitBarrierDir = directory
	return nil
}

func (s *Service) waitAtResultCommitBarrier(ctx context.Context, event *transportv1.TransportEvent, observedAt time.Time) error {
	if s.resultCommitBarrierDir == "" || event.GetType() != transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT {
		return nil
	}
	armed, err := os.ReadFile(filepath.Join(s.resultCommitBarrierDir, "arm"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read result commit barrier: %w", err)
	}
	var result agentv1.CommandResult
	if err := proto.Unmarshal(event.GetPayload(), &result); err != nil {
		return nil
	}
	commandID, err := uuid.FromBytes(result.GetCommandId())
	if err != nil || string(bytes.TrimSpace(armed)) != commandID.String() {
		return nil
	}
	received := []byte(commandID.String() + "\n" + observedAt.UTC().Format(time.RFC3339) + "\n")
	if err := os.WriteFile(filepath.Join(s.resultCommitBarrierDir, "received"), received, 0o600); err != nil {
		return fmt.Errorf("signal result commit barrier: %w", err)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			release, readErr := os.ReadFile(filepath.Join(s.resultCommitBarrierDir, "release"))
			if readErr == nil && string(bytes.TrimSpace(release)) == commandID.String() {
				return nil
			}
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return fmt.Errorf("read result commit barrier release: %w", readErr)
			}
		}
	}
}

func invalidCommandResult(detail string) error {
	return invalidEvent("invalid_command_result", detail)
}

func normalizeCertificateCSRResult(envelope *agentv1.CommandEnvelope, state string, resultBytes []byte) (*agentv1.CertificateCsrResult, error) {
	request := envelope.GetCertificateCsr()
	if request == nil || state != "succeeded" {
		return nil, nil
	}
	var result agentv1.CertificateCsrResult
	if len(resultBytes) == 0 || proto.Unmarshal(resultBytes, &result) != nil {
		return nil, errors.New("certificate CSR result is malformed")
	}
	if !bytes.Equal(result.GetCertificateId(), request.GetCertificateId()) || len(result.GetCsrDer()) < 64 || len(result.GetCsrDer()) > 64*1024 || len(result.GetPublicKeySha256()) != sha256.Size {
		return nil, errors.New("certificate CSR result is inconsistent")
	}
	csr, err := x509.ParseCertificateRequest(result.GetCsrDer())
	if err != nil || csr.CheckSignature() != nil {
		return nil, errors.New("certificate CSR signature is invalid")
	}
	key, ok := csr.PublicKey.(*rsa.PublicKey)
	requestedNames, actualNames := slices.Clone(request.GetDnsNames()), slices.Clone(csr.DNSNames)
	sort.Strings(requestedNames)
	sort.Strings(actualNames)
	if !ok || uint32(key.N.BitLen()) != request.GetKeyBits() || csr.Subject.CommonName != request.GetCommonName() || len(csr.Subject.Names) != 1 || !csr.Subject.Names[0].Type.Equal([]int{2, 5, 4, 3}) || !slices.Equal(requestedNames, actualNames) || len(csr.EmailAddresses) != 0 || len(csr.IPAddresses) != 0 || len(csr.URIs) != 0 {
		return nil, errors.New("certificate CSR identity does not match the requested subject")
	}
	for _, extension := range csr.Extensions {
		if !extension.Id.Equal([]int{2, 5, 29, 17}) {
			return nil, errors.New("certificate CSR contains an unsupported extension")
		}
	}
	publicKey, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return nil, errors.New("certificate CSR public key is invalid")
	}
	digest := sha256.Sum256(publicKey)
	if !bytes.Equal(result.GetPublicKeySha256(), digest[:]) {
		return nil, errors.New("certificate CSR public key digest mismatch")
	}
	return &result, nil
}

func normalizeCertificateRevokeResult(envelope *agentv1.CommandEnvelope, state string, resultBytes []byte) (*agentv1.CertificateRevokeResult, error) {
	request := envelope.GetCertificateRevoke()
	if request == nil || state != "succeeded" {
		return nil, nil
	}
	var result agentv1.CertificateRevokeResult
	if len(resultBytes) == 0 || proto.Unmarshal(resultBytes, &result) != nil || !bytes.Equal(result.GetCertificateId(), request.GetCertificateId()) || !result.GetKeyRemoved() {
		return nil, errors.New("certificate revoke result is inconsistent")
	}
	return &result, nil
}

func normalizeCertificateArtifactResult(envelope *agentv1.CommandEnvelope, state string, resultBytes []byte) (*agentv1.CertificateArtifactResult, error) {
	request := envelope.GetCertificateP12()
	if request == nil || state != "succeeded" {
		return nil, nil
	}
	var result agentv1.CertificateArtifactResult
	if len(resultBytes) == 0 || proto.Unmarshal(resultBytes, &result) != nil || !bytes.Equal(result.GetCertificateId(), request.GetCertificateId()) || !bytes.Equal(result.GetArtifactId(), request.GetArtifactId()) || len(result.GetArtifactSha256()) != sha256.Size || result.GetArtifactSize() == 0 || result.GetArtifactSize() > 64*1024*1024 {
		return nil, errors.New("certificate artifact result is inconsistent")
	}
	return &result, nil
}

func normalizeConfigApplyResult(envelope *agentv1.CommandEnvelope, state string, resultBytes []byte) (string, *agentv1.ConfigApplyResult, error) {
	apply := envelope.GetConfigApply()
	if apply == nil || state != "succeeded" {
		return state, nil, nil
	}
	var result agentv1.ConfigApplyResult
	if len(resultBytes) == 0 || proto.Unmarshal(resultBytes, &result) != nil {
		return "", nil, errors.New("configuration apply result is malformed")
	}
	if !bytes.Equal(result.GetCandidateHash(), apply.GetCandidateHash()) || !bytes.Equal(result.GetPreviousHash(), apply.GetExpectedCurrentHash()) {
		return "", nil, errors.New("configuration apply result hash mismatch")
	}
	switch {
	case result.GetHealthy() && !result.GetRolledBack() && !result.GetFailedCritical() && result.GetFailureCode() == "" &&
		bytes.Equal(result.GetObservedHash(), apply.GetCandidateHash()) && result.GetAppliedRevision() == apply.GetDesiredRevision():
		return "succeeded", &result, nil
	case result.GetHealthy() && result.GetRolledBack() && !result.GetFailedCritical() && result.GetAppliedRevision() == 0 &&
		bytes.Equal(result.GetObservedHash(), apply.GetExpectedCurrentHash()) &&
		(result.GetFailureCode() == "health_check_failed" || result.GetFailureCode() == "recovered_health_check_failed"):
		return "rolled_back", &result, nil
	case !result.GetHealthy() && !result.GetRolledBack() && result.GetFailedCritical() && result.GetAppliedRevision() == 0 && len(result.GetObservedHash()) == 0 &&
		(result.GetFailureCode() == "rollback_failed" || result.GetFailureCode() == "recovery_rollback_failed"):
		return "failed", &result, nil
	default:
		return "", nil, errors.New("configuration apply result has an invalid outcome")
	}
}

// validateAgentUpgradeScheduledResult checks that a succeeded agent upgrade
// result carries the scheduling acknowledgement bound to this exact command.
// The acknowledgement proves scheduling only; the reconciled terminal outcome
// comes later from durable evidence.
func validateAgentUpgradeScheduledResult(envelope *agentv1.CommandEnvelope, resultBytes []byte) error {
	upgrade := envelope.GetAgentUpgrade()
	var scheduled agentv1.AgentUpgradeScheduledResult
	if len(resultBytes) == 0 || proto.Unmarshal(resultBytes, &scheduled) != nil {
		return errors.New("agent upgrade scheduled result is malformed")
	}
	if !bytes.Equal(scheduled.GetOperationId(), envelope.GetOperationId()) ||
		scheduled.GetTargetVersion() != upgrade.GetTargetVersion() ||
		!bytes.Equal(scheduled.GetPackageSha256(), upgrade.GetPackageSha256()) {
		return errors.New("agent upgrade scheduled result does not match the release identity")
	}
	return nil
}

func commandAuditAction(envelope *agentv1.CommandEnvelope) string {
	switch envelope.GetPayload().(type) {
	case *agentv1.CommandEnvelope_SessionDisconnect:
		return "session.disconnect"
	case *agentv1.CommandEnvelope_SessionTerminate:
		return "session.terminate"
	case *agentv1.CommandEnvelope_IpBanRemove:
		return "ip_ban.remove"
	case *agentv1.CommandEnvelope_ServiceReload:
		return "service.reload"
	case *agentv1.CommandEnvelope_UserCreate:
		return "user.create"
	case *agentv1.CommandEnvelope_UserDisable:
		return "user.disable"
	case *agentv1.CommandEnvelope_UserEnable:
		return "user.enable"
	case *agentv1.CommandEnvelope_UserPasswordRotate:
		return "user.password.rotate"
	case *agentv1.CommandEnvelope_GroupApply:
		return "group.apply"
	case *agentv1.CommandEnvelope_ConfigPlan:
		return "config.plan"
	case *agentv1.CommandEnvelope_ConfigApply:
		return "config.apply"
	case *agentv1.CommandEnvelope_CertificateCsr:
		return "certificate.csr.generate"
	case *agentv1.CommandEnvelope_CertificateP12:
		return "certificate.private_key.export"
	case *agentv1.CommandEnvelope_CertificateRevoke:
		return "certificate.revoke"
	case *agentv1.CommandEnvelope_AgentUpgrade:
		return "agent.upgrade"
	default:
		return "synthetic.command"
	}
}

func agentPayloadHash(envelope *agentv1.CommandEnvelope) ([sha256.Size]byte, error) {
	var capability string
	var payload []byte
	var err error
	switch {
	case envelope.GetSyntheticNoop() != nil:
		capability = "synthetic.noop"
		payload, err = proto.Marshal(envelope.GetSyntheticNoop())
	case envelope.GetSyntheticEcho() != nil:
		capability = "synthetic.echo"
		payload, err = proto.Marshal(envelope.GetSyntheticEcho())
	default:
		return [sha256.Size]byte{}, errors.New("command result payload type is not reconcilable")
	}
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("encode command payload for result verification: %w", err)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(capability))
	_, _ = hash.Write(envelope.GetNodeId())
	var revision [8]byte
	binary.BigEndian.PutUint64(revision[:], envelope.GetExpectedRevision())
	_, _ = hash.Write(revision[:])
	_, _ = hash.Write(payload)
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func newIDs() (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, error) {
	ids := make([]uuid.UUID, 5)
	for index := range ids {
		id, err := uuid.NewV7()
		if err != nil {
			return uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("generate UUIDv7: %w", err)
		}
		ids[index] = id
	}
	return ids[0], ids[1], ids[2], ids[3], ids[4], nil
}

func normalizeScenario(scenario Scenario) (uint32, uint32, error) {
	heartbeatCount := uint32(3)
	if scenario.HeartbeatCount != nil {
		heartbeatCount = *scenario.HeartbeatCount
	}
	delayMillis := uint32(100)
	if scenario.DelayMillis != nil {
		delayMillis = *scenario.DelayMillis
	}
	if heartbeatCount < 1 || heartbeatCount > 32 || delayMillis > 30_000 {
		return 0, 0, fmt.Errorf("%w: simulation limits exceeded", ErrInvalidScenario)
	}
	return heartbeatCount, delayMillis, nil
}

func marshalEnvelope(nodeID, operationID, commandID uuid.UUID, traceparent string, scenario Scenario, heartbeatCount, delayMillis uint32, now time.Time) ([]byte, error) {
	messageID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate message ID: %w", err)
	}
	envelope := &agentv1.CommandEnvelope{
		ProtocolVersion: "1.0", MessageId: messageID[:], CommandId: commandID[:], IdempotencyKey: operationID[:], NodeId: nodeID[:],
		Sequence: 1, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(time.Minute)), Traceparent: traceparent,
		ActorId: "developer", Reason: "I03 local side-effect-free slice", DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY,
		Payload: &agentv1.CommandEnvelope_SimulationProbe{SimulationProbe: &agentv1.SimulationProbe{
			HeartbeatCount: heartbeatCount, DelayMillis: delayMillis, DuplicateEvent: scenario.DuplicateEvent,
			ReturnError: scenario.ReturnError, DisconnectAfter: scenario.DisconnectAfter,
		}},
	}
	data, err := proto.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("marshal simulator command: %w", err)
	}
	return data, nil
}

func eventName(value transportv1.TransportEventType) (string, error) {
	switch value {
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_CONNECTED:
		return "connected", nil
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_DISCONNECTED:
		return "disconnected", nil
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT:
		return "command_result", nil
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_SIMULATION_RESULT:
		return "simulation_result", nil
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT:
		return "heartbeat", nil
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_ERROR:
		return "error", nil
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_PATH_CHANGED:
		return "path_changed", nil
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY:
		return "telemetry", nil
	default:
		return "", errors.New("unsupported transport event type")
	}
}

func connectedOwnerTerm(event *transportv1.TransportEvent) ([16]byte, bool, error) {
	connection := event.GetConnectionId()
	epoch := event.GetOwnerEpoch()
	if len(connection) == 0 && epoch == 0 {
		return [16]byte{}, false, nil
	}
	if len(connection) != 16 || epoch == 0 || epoch > math.MaxInt64 {
		return [16]byte{}, false, errors.New("connected owner term must contain a 16-byte connection_id and positive owner_epoch")
	}
	id, err := uuid.FromBytes(connection)
	if err != nil || id.Version() != 7 {
		return [16]byte{}, false, errors.New("connected owner connection_id must be UUIDv7")
	}
	var fixed [16]byte
	copy(fixed[:], connection)
	return fixed, true, nil
}

func validTraceparent(value string) bool {
	parts := [4]string{}
	count := 0
	var start int
	for index := 0; index <= len(value); index++ {
		if index == len(value) || value[index] == '-' {
			if count >= len(parts) {
				return false
			}
			parts[count] = value[start:index]
			count++
			start = index + 1
		}
	}
	if count != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return false
	}
	for _, part := range parts {
		for index := range len(part) {
			character := part[index]
			if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
				return false
			}
		}
		if len(part)%2 != 0 {
			return false
		}
	}
	return parts[1] != "00000000000000000000000000000000" && parts[2] != "0000000000000000"
}

func traceID(traceparent string) string { return traceparent[3:35] }
