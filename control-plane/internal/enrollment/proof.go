package enrollment

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
)

const (
	EnrollmentProofVersionV1 = 1
	EnrollmentProtocolMajor  = 1
	EnrollmentProtocolMinor  = 0
)

var enrollmentProofDomainV1 = []byte("ocservia/agent-enrollment/v1\x00")

// EnrollmentCanonicalV1 returns the independent, domain-separated signing
// input for EndpointID proof of possession. It never serializes Protobuf.
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
	tokenHash := sha256.Sum256([]byte(request.GetToken()))
	var encoded bytes.Buffer
	encoded.Grow(1024)
	encoded.Write(enrollmentProofDomainV1)
	writeEnrollmentUint32(&encoded, EnrollmentProofVersionV1)
	writeEnrollmentUint32(&encoded, request.GetEnrollmentProtocolMajor())
	writeEnrollmentUint32(&encoded, request.GetEnrollmentProtocolMinor())
	encoded.Write(tokenHash[:])
	encoded.Write(request.GetEndpointId())
	for _, value := range []string{request.GetAgentVersion(), request.GetOsRelease(), request.GetOcservVersion(), request.GetBootId()} {
		if err := writeEnrollmentBytes(&encoded, []byte(value)); err != nil {
			return nil, err
		}
	}
	encoded.Write(request.GetAgentInstanceId())
	writeEnrollmentUint32(&encoded, uint32(len(capabilities)))
	for _, capability := range capabilities {
		if err := writeEnrollmentBytes(&encoded, []byte(capability)); err != nil {
			return nil, err
		}
	}
	if err := writeEnrollmentBytes(&encoded, []byte(request.GetEnvironment())); err != nil {
		return nil, err
	}
	if err := writeEnrollmentBytes(&encoded, request.GetNonce()); err != nil {
		return nil, err
	}
	writeEnrollmentUint64(&encoded, uint64(request.GetTime().Seconds))
	writeEnrollmentUint32(&encoded, uint32(request.GetTime().Nanos))
	return encoded.Bytes(), nil
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

func writeEnrollmentBytes(buffer *bytes.Buffer, value []byte) error {
	if uint64(len(value)) > math.MaxUint32 {
		return errors.New("enrollment proof field exceeds uint32")
	}
	writeEnrollmentUint32(buffer, uint32(len(value)))
	buffer.Write(value)
	return nil
}

func writeEnrollmentUint32(buffer *bytes.Buffer, value uint32) {
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], value)
	buffer.Write(raw[:])
}

func writeEnrollmentUint64(buffer *bytes.Buffer, value uint64) {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], value)
	buffer.Write(raw[:])
}
