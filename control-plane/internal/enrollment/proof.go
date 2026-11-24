package enrollment

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/canonicalcbor"
)

const (
	EnrollmentProofVersionV1 = 1
	EnrollmentProtocolMajor  = 1
	EnrollmentProtocolMinor  = 1

	enrollmentProofDomainV1 = "ocservia/agent-enrollment/cbor.v1\x00"
)

type cborSealingKey struct {
	Version         uint32 `cbor:"version"`
	Purpose         uint32 `cbor:"purpose"`
	KeyID           string `cbor:"key_id"`
	PublicKeySHA256 []byte `cbor:"public_key_sha256"`
}

type cborEnrollmentProofV1 struct {
	ProofVersion    uint32           `cbor:"proof_version"`
	ProtocolMajor   uint32           `cbor:"protocol_major"`
	ProtocolMinor   uint32           `cbor:"protocol_minor"`
	TokenSHA256     []byte           `cbor:"token_sha256"`
	EndpointID      []byte           `cbor:"endpoint_id"`
	AgentVersion    string           `cbor:"agent_version"`
	OSRelease       string           `cbor:"os_release"`
	OcservVersion   string           `cbor:"ocserv_version"`
	BootID          string           `cbor:"boot_id"`
	AgentInstanceID []byte           `cbor:"agent_instance_id"`
	Capabilities    []string         `cbor:"capabilities"`
	SealingKeys     []cborSealingKey `cbor:"sealing_keys"`
	Environment     string           `cbor:"environment"`
	Nonce           []byte           `cbor:"nonce"`
	TimeSeconds     int64            `cbor:"time_seconds"`
	TimeNanos       uint32           `cbor:"time_nanos"`
}

// EnrollmentCanonicalV1 returns the independent, domain-separated RFC 8949 Core
// Deterministic CBOR signing input for EndpointID proof of possession.
func EnrollmentCanonicalV1(request *agentv1.EnrollRequest) ([]byte, error) {
	if request == nil || request.GetEnrollmentProtocolMajor() != EnrollmentProtocolMajor ||
		request.GetEnrollmentProtocolMinor() != EnrollmentProtocolMinor || len(request.GetEndpointId()) != ed25519.PublicKeySize ||
		request.GetTime() == nil || request.GetTime().CheckValid() != nil || request.GetTime().Nanos < 0 ||
		len(request.GetAgentInstanceId()) != 16 || len(request.GetNonce()) < 16 || len(request.GetNonce()) > 64 {
		return nil, errors.New("enrollment proof claims are invalid")
	}
	capabilities := slices.Clone(request.GetCapabilities())
	slices.Sort(capabilities)
	if len(capabilities) == 0 || len(capabilities) > 128 {
		return nil, errors.New("enrollment proof capabilities are invalid")
	}
	for index, capability := range capabilities {
		if capability == "" || len(capability) > 128 || (index > 0 && capabilities[index-1] == capability) {
			return nil, errors.New("enrollment proof capabilities are invalid")
		}
	}
	sealingKeys := slices.Clone(request.GetSealingKeys())
	slices.SortFunc(sealingKeys, func(a, b *agentv1.SealingKeyDescriptorV1) int { return int(a.GetPurpose() - b.GetPurpose()) })
	if err := validateSealingKeys(sealingKeys); err != nil {
		return nil, err
	}
	tokenHash := sha256.Sum256([]byte(request.GetToken()))
	keys := make([]cborSealingKey, 0, len(sealingKeys))
	for _, key := range sealingKeys {
		keys = append(keys, cborSealingKey{
			Version:         key.GetVersion(),
			Purpose:         uint32(key.GetPurpose()),
			KeyID:           key.GetKeyId(),
			PublicKeySHA256: key.GetPublicKeySha256(),
		})
	}
	return canonicalcbor.Marshal(enrollmentProofDomainV1, cborEnrollmentProofV1{
		ProofVersion:    EnrollmentProofVersionV1,
		ProtocolMajor:   request.GetEnrollmentProtocolMajor(),
		ProtocolMinor:   request.GetEnrollmentProtocolMinor(),
		TokenSHA256:     tokenHash[:],
		EndpointID:      request.GetEndpointId(),
		AgentVersion:    request.GetAgentVersion(),
		OSRelease:       request.GetOsRelease(),
		OcservVersion:   request.GetOcservVersion(),
		BootID:          request.GetBootId(),
		AgentInstanceID: request.GetAgentInstanceId(),
		Capabilities:    capabilities,
		SealingKeys:     keys,
		Environment:     request.GetEnvironment(),
		Nonce:           request.GetNonce(),
		TimeSeconds:     request.GetTime().GetSeconds(),
		TimeNanos:       uint32(request.GetTime().GetNanos()),
	})
}

func validateSealingKeys(keys []*agentv1.SealingKeyDescriptorV1) error {
	if len(keys) != 2 || keys[0] == nil || keys[1] == nil || keys[0].GetPurpose() != agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD || keys[1].GetPurpose() != agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD {
		return errors.New("enrollment sealing keys are invalid")
	}
	for _, key := range keys {
		if key.GetVersion() != 1 || !validKeyID(key.GetKeyId()) || len(key.GetPublicKeySha256()) != sha256.Size {
			return errors.New("enrollment sealing keys are invalid")
		}
	}
	if keys[0].GetKeyId() == keys[1].GetKeyId() || bytes.Equal(keys[0].GetPublicKeySha256(), keys[1].GetPublicKeySha256()) {
		return errors.New("password sealing keys must be distinct")
	}
	return nil
}

func validKeyID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, value := range []byte(value) {
		if !(value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '.' || value == '_' || value == '-') {
			return false
		}
	}
	return true
}

func verifyEnrollmentProof(request *agentv1.EnrollRequest) error {
	proof := request.GetProof()
	if proof == nil || proof.GetVersion() != EnrollmentProofVersionV1 || len(proof.GetSignature()) != ed25519.SignatureSize {
		return ErrEndpointProof
	}
	canonical, err := EnrollmentCanonicalV1(request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrEndpointProof, err)
	}
	if !ed25519.Verify(ed25519.PublicKey(request.GetEndpointId()), canonical, proof.GetSignature()) {
		return ErrEndpointProof
	}
	return nil
}
