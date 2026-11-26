package transportevents

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"math"
	"slices"
	"sort"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	eventstore "github.com/GentleKingson/ocservia/control-plane/internal/transportevents/store"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

type Event = eventstore.Event

type IngressHook func(ctx context.Context, tx database.Tx, nodeID uuid.UUID, eventType, traceparent string, occurredAt time.Time) error
type GapHook func(ctx context.Context, readConnection transportclient.NodeConnectionReader) (func(context.Context, database.Tx) error, error)

type Service struct {
	backend           database.Backend
	now               func() time.Time
	signer            *commandauth.Signer
	commandRecovery   *operations.Service
	recoveryAuthority *ownersession.Manager
	resultCommitFault
	ingressHook IngressHook
	gapHook     GapHook
}

const (
	maxTransportEventAge = telemetry.MaxTelemetryAge
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

func New(backend database.Backend, signer *commandauth.Signer) *Service {
	return &Service{backend: backend, signer: signer, now: func() time.Time { return time.Now().UTC() }}
}

func NewWithCommandRecovery(backend database.Backend, signer *commandauth.Signer, recovery *operations.Service, authority *ownersession.Manager) *Service {
	service := New(backend, signer)
	service.commandRecovery, service.recoveryAuthority = recovery, authority
	return service
}

// SetHooks attaches optional development hooks for ingress projection and
// retention-gap reconciliation. Production leaves both nil.
func (s *Service) SetHooks(onIngress IngressHook, onGap GapHook) {
	s.ingressHook, s.gapHook = onIngress, onGap
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

type configApplyIdentity struct {
	candidateHash, currentHash, materializedHash []byte
	desiredRevision                              uint64
}

func configApplyBinding(envelope *agentv1.CommandEnvelope) *configApplyIdentity {
	if apply := envelope.GetConfigApply(); apply != nil {
		return &configApplyIdentity{apply.GetCandidateHash(), apply.GetExpectedCurrentHash(), apply.GetCandidateHash(), apply.GetDesiredRevision()}
	}
	if apply := envelope.GetCompleteConfigApply(); apply != nil {
		return &configApplyIdentity{apply.GetCandidateHash(), apply.GetExpectedCurrentHash(), apply.GetMaterializedHash(), apply.GetDesiredRevision()}
	}
	return nil
}

func normalizeConfigApplyResult(envelope *agentv1.CommandEnvelope, state string, resultBytes []byte) (string, *agentv1.ConfigApplyResult, error) {
	apply := configApplyBinding(envelope)
	if apply == nil || state != "succeeded" {
		return state, nil, nil
	}
	var result agentv1.ConfigApplyResult
	if len(resultBytes) == 0 || proto.Unmarshal(resultBytes, &result) != nil {
		return "", nil, errors.New("configuration apply result is malformed")
	}
	if !bytes.Equal(result.GetCandidateHash(), apply.candidateHash) || !bytes.Equal(result.GetPreviousHash(), apply.currentHash) {
		return "", nil, errors.New("configuration apply result hash mismatch")
	}
	switch {
	case result.GetHealthy() && !result.GetRolledBack() && !result.GetFailedCritical() && result.GetFailureCode() == "" &&
		bytes.Equal(result.GetObservedHash(), apply.materializedHash) && result.GetAppliedRevision() == apply.desiredRevision:
		return "succeeded", &result, nil
	case result.GetHealthy() && result.GetRolledBack() && !result.GetFailedCritical() && result.GetAppliedRevision() == 0 &&
		bytes.Equal(result.GetObservedHash(), apply.currentHash) &&
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
	case *agentv1.CommandEnvelope_ConfigPlan, *agentv1.CommandEnvelope_CompleteConfigPlan:
		return "config.plan"
	case *agentv1.CommandEnvelope_ConfigApply, *agentv1.CommandEnvelope_CompleteConfigApply:
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
	epoch := event.GetSessionEpoch()
	if len(connection) == 0 && epoch == 0 {
		return [16]byte{}, false, nil
	}
	if len(connection) != 16 || epoch == 0 || epoch > math.MaxInt64 {
		return [16]byte{}, false, errors.New("connected owner term must contain a 16-byte connection_id and positive session_epoch")
	}
	id, err := uuid.FromBytes(connection)
	if err != nil || id.Version() != 7 {
		return [16]byte{}, false, errors.New("connected owner connection_id must be UUIDv7")
	}
	var fixed [16]byte
	copy(fixed[:], connection)
	return fixed, true, nil
}
