package operations

import (
	"crypto/sha256"
	"net"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	tracecontext "github.com/GentleKingson/ocservia/control-plane/internal/platform/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Payload is closed to the operation kinds admitted by Create. Callers supply
// a value, not a pointer; validation rejects nil and pointer payloads.
type Payload interface{ kind() Kind }

type SyntheticNoopPayload struct{}
type SyntheticEchoPayload struct{ Message string }
type SessionDisconnectPayload struct{ SessionID, BootID string }
type SessionTerminatePayload struct{ SessionID, BootID string }
type IPBanRemovePayload struct{ IP string }
type ServiceReloadPayload struct{}

type ConfigPlanPayload struct {
	Candidate     configprofile.Normalized
	PlanMetadata  *ConfigPlanMetadata
	OcservVersion string
}

type ConfigApplyPayload struct {
	Candidate           configprofile.Normalized
	ExpectedCurrentHash []byte
	DesiredRevision     uint64
	ApplyMetadata       *ConfigApplyMetadata
	MaterializedHash    []byte
	PlanExpiresAt       *timestamppb.Timestamp
}

type CertificateCSRPayload struct {
	CertificateID uuid.UUID
	CommonName    string
	DNSNames      []string
	KeyBits       uint32
}

type CertificateP12Payload struct {
	CertificateID      uuid.UUID
	CertificateVersion uint64
	CertificateChain   []byte
	SealedPassword     *agentv1.SealedSecretV1
	ArtifactID         uuid.UUID
	ArtifactMetadata   *ArtifactMetadata
}

type CertificateRevokePayload struct {
	CertificateID      uuid.UUID
	CertificateVersion uint64
	RevocationReason   string
	HoldDispatch       bool
}

type AgentUpgradePayload struct {
	TargetVersion string
	PackageSHA256 []byte
	Architecture  string
	// RolloutID binds generated upgrades to their durable fleet rollout.
	// Dispatch validates the stored intent independently of the browser session.
	RolloutID uuid.UUID
}

func (SyntheticNoopPayload) kind() Kind     { return SyntheticNoop }
func (SyntheticEchoPayload) kind() Kind     { return SyntheticEcho }
func (SessionDisconnectPayload) kind() Kind { return SessionDisconnect }
func (SessionTerminatePayload) kind() Kind  { return SessionTerminate }
func (IPBanRemovePayload) kind() Kind       { return IPBanRemove }
func (ServiceReloadPayload) kind() Kind     { return ServiceReload }
func (ConfigPlanPayload) kind() Kind        { return ConfigPlan }
func (ConfigApplyPayload) kind() Kind       { return ConfigApply }
func (CertificateCSRPayload) kind() Kind    { return CertificateCSR }
func (CertificateP12Payload) kind() Kind    { return CertificateP12 }
func (CertificateRevokePayload) kind() Kind { return CertificateRevoke }
func (AgentUpgradePayload) kind() Kind      { return AgentUpgrade }

func validateCreate(r CreateRequest) error {
	if r.NodeID == uuid.Nil || r.NodeID.Version() != 7 || r.ExpectedVersion < 1 || r.RequestID == "" || tracecontext.TraceID(r.Traceparent) == "" || r.TTL < time.Second || r.TTL > 24*time.Hour {
		return ErrInvalidRequest
	}
	if len(r.IdempotencyKey) < 1 || len(r.IdempotencyKey) > 128 || strings.TrimSpace(r.IdempotencyKey) != r.IdempotencyKey {
		return ErrInvalidRequest
	}
	// Explicit value cases also reject typed nil pointers before invoking kind.
	switch p := r.Payload.(type) {
	case SyntheticNoopPayload:
	case SyntheticEchoPayload:
		if len(p.Message) > 4096 {
			return ErrInvalidRequest
		}
	case SessionDisconnectPayload:
		if !validSessionTarget(p.SessionID, p.BootID) {
			return ErrInvalidRequest
		}
	case SessionTerminatePayload:
		if !validSessionTarget(p.SessionID, p.BootID) {
			return ErrInvalidRequest
		}
	case IPBanRemovePayload:
		parsed := net.ParseIP(p.IP)
		if parsed == nil || parsed.String() != p.IP {
			return ErrInvalidRequest
		}
	case ServiceReloadPayload:
		if r.Action != "service.reload" || !hasActorSession(r) {
			return ErrInvalidRequest
		}
	case ConfigPlanPayload:
		if !p.Candidate.IsFor(r.NodeID) || p.PlanMetadata == nil || strings.TrimSpace(p.PlanMetadata.TemplateName) == "" || len(p.PlanMetadata.TemplateName) > 128 || len(p.PlanMetadata.CandidateRedacted) == 0 || len(p.PlanMetadata.CandidateRedacted) > 256*1024 || r.ActorID == "" {
			return ErrInvalidRequest
		}
	case ConfigApplyPayload:
		if r.Action != "config.apply" || !hasActorSession(r) || !p.Candidate.IsFor(r.NodeID) || len(p.ExpectedCurrentHash) != sha256.Size || p.DesiredRevision <= p.Candidate.Revision() || p.ApplyMetadata == nil || p.ApplyMetadata.PlanID == uuid.Nil || len(p.MaterializedHash) != sha256.Size || p.PlanExpiresAt == nil || !p.PlanExpiresAt.IsValid() {
			return ErrInvalidRequest
		}
	case CertificateCSRPayload:
		if p.CertificateID == uuid.Nil || p.CertificateID.Version() != 7 || !validDNSName(p.CommonName) || len(p.DNSNames) > 32 || (p.KeyBits != 2048 && p.KeyBits != 3072 && p.KeyBits != 4096) {
			return ErrInvalidRequest
		}
		for _, name := range p.DNSNames {
			if !validDNSName(name) {
				return ErrInvalidRequest
			}
		}
	case CertificateP12Payload:
		if r.Action != "certificate.private_key.export" || r.ActorIdentityID == uuid.Nil || p.CertificateID == uuid.Nil || p.CertificateID.Version() != 7 || p.CertificateVersion == 0 || p.ArtifactID == uuid.Nil || p.ArtifactID.Version() != 7 || len(p.CertificateChain) < 64 || len(p.CertificateChain) > 256*1024 || !validSealedP12(p.SealedPassword) || p.ArtifactMetadata == nil || len(p.ArtifactMetadata.TokenSHA256) != sha256.Size || !p.ArtifactMetadata.ExpiresAt.After(time.Now().UTC()) || p.ArtifactMetadata.ExpiresAt.After(time.Now().UTC().Add(30*time.Minute)) {
			return ErrInvalidRequest
		}
	case CertificateRevokePayload:
		if r.Action != "certificate.revoke" || r.ActorIdentityID == uuid.Nil || p.CertificateID == uuid.Nil || p.CertificateID.Version() != 7 || p.CertificateVersion == 0 || strings.TrimSpace(p.RevocationReason) == "" || len(p.RevocationReason) > 128 {
			return ErrInvalidRequest
		}
	case AgentUpgradePayload:
		if r.Action != "agent.upgrade" || !hasActorSession(r) || !semanticpayload.ValidAgentUpgradeTargetVersion(p.TargetVersion) || len(p.PackageSHA256) != sha256.Size || !semanticpayload.ValidAgentUpgradeArchitecture(p.Architecture) {
			return ErrInvalidRequest
		}
		if p.RolloutID != uuid.Nil && p.RolloutID.Version() != 7 {
			return ErrInvalidRequest
		}
	default:
		return ErrInvalidRequest
	}
	switch r.Payload.(type) {
	case SyntheticNoopPayload, SyntheticEchoPayload:
	default:
		if r.Action == "" || r.Reason == "" || len(r.Reason) > 512 {
			return ErrInvalidRequest
		}
	}
	return nil
}

func hasActorSession(r CreateRequest) bool {
	return r.ActorID != "" && r.ActorIdentityID != uuid.Nil && r.ActorSessionID != uuid.Nil
}

func validSessionTarget(sessionID, bootID string) bool {
	id, err := strconv.ParseUint(sessionID, 10, 64)
	if err != nil || id == 0 || strconv.FormatUint(id, 10) != sessionID {
		return false
	}
	boot, err := uuid.Parse(bootID)
	return err == nil && boot != uuid.Nil
}

func sessionTarget(p Payload) (sessionID, bootID string, ok bool) {
	switch p := p.(type) {
	case SessionDisconnectPayload:
		return p.SessionID, p.BootID, true
	case SessionTerminatePayload:
		return p.SessionID, p.BootID, true
	default:
		return "", "", false
	}
}

func requestCapability(r CreateRequest) string {
	switch r.Payload.(type) {
	case SessionDisconnectPayload:
		return "ocserv.session.disconnect"
	case SessionTerminatePayload:
		return "ocserv.session.terminate"
	case IPBanRemovePayload:
		return "ocserv.ip_ban.remove"
	case ServiceReloadPayload:
		return "ocserv.service.reload"
	case ConfigPlanPayload:
		return configprofile.PlanCapability
	case ConfigApplyPayload:
		return configprofile.ApplyCapability
	case CertificateCSRPayload, CertificateP12Payload:
		return "ocserv.certificate.issue"
	case CertificateRevokePayload:
		return "ocserv.certificate.revoke"
	case AgentUpgradePayload:
		return "ocserv.agent.upgrade.v2"
	default:
		return ""
	}
}
