package enrollment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var endpointPrivateKeys sync.Map

func createToken(t *testing.T, service *Service, workspaceID uuid.UUID, endpoint []byte) Token {
	t.Helper()
	token, err := service.CreateBootstrapToken(context.Background(), BootstrapTokenSpec{
		WorkspaceID:        workspaceID,
		Environment:        "test",
		ExpectedEndpointID: endpoint,
		ActorID:            "integration",
		Reason:             "test enrollment",
		RequestID:          uuid.Must(uuid.NewV7()).String(),
	})
	testutil.NoError(t, err)
	return token
}

func enrollmentRequest(token string, endpoint []byte) *agentv1.EnrollRequest {
	return enrollmentRequestCapabilities(token, endpoint, []string{"ocserv.status.read"})
}

func enrollmentRequestCapabilities(token string, endpoint []byte, capabilities []string) *agentv1.EnrollRequest {
	request := &agentv1.EnrollRequest{
		Token:                   token,
		EndpointId:              endpoint,
		AgentVersion:            "test",
		OsRelease:               "test",
		OcservVersion:           "test",
		BootId:                  "boot",
		AgentInstanceId:         testfixture.UUIDBytes(),
		Capabilities:            capabilities,
		Environment:             "test",
		Nonce:                   make([]byte, 16),
		Time:                    timestamppb.Now(),
		EnrollmentProtocolMajor: EnrollmentProtocolMajor,
		EnrollmentProtocolMinor: EnrollmentProtocolMinor,
		SealingKeys:             enrollmentSealingKeys(),
	}
	privateKey, ok := endpointPrivateKeys.Load(string(request.GetEndpointId()))
	if !ok {
		panic("missing enrollment endpoint private key fixture")
	}
	canonical, err := EnrollmentCanonicalV1(request)
	if err != nil {
		panic(err)
	}
	request.Proof = &agentv1.EnrollmentProofV1{
		Version:   EnrollmentProofVersionV1,
		Signature: ed25519.Sign(privateKey.(ed25519.PrivateKey), canonical),
	}
	return request
}

func enrollmentSealingKeys() []*agentv1.SealingKeyDescriptorV1 {
	return []*agentv1.SealingKeyDescriptorV1{
		{Version: agentv1.SealedSecretVersion_SEALED_SECRET_VERSION_V1, Purpose: agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD, KeyId: "fixture-user-key-v1", PublicKeySha256: bytes.Repeat([]byte{0x11}, 32)},
		{Version: agentv1.SealedSecretVersion_SEALED_SECRET_VERSION_V1, Purpose: agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD, KeyId: "fixture-p12-key-v1", PublicKeySha256: bytes.Repeat([]byte{0x22}, 32)},
	}
}

func endpointFixture(seed byte) []byte {
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	publicKey := slices.Clone(privateKey.Public().(ed25519.PublicKey))
	endpointPrivateKeys.Store(string(publicKey), privateKey)
	return publicKey
}

type cancellingRegistrar struct {
	cancelled context.CancelFunc
	fired     atomic.Bool
}

func (r *cancellingRegistrar) RegisterOwnerFence(_ context.Context, _ *agentv1.ConnectionFenceV2) error {
	if r.fired.CompareAndSwap(false, true) {
		r.cancelled()
	}
	return nil
}

type recordingRegistrar struct{}

func (r *recordingRegistrar) RegisterOwnerFence(_ context.Context, _ *agentv1.ConnectionFenceV2) error {
	return nil
}
