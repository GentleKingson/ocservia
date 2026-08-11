package enrollment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCreateTokenRequiresExpectedEndpoint(t *testing.T) {
	service := &Service{}
	_, err := service.CreateToken(context.Background(), TokenSpec{
		WorkspaceID: uuid.Must(uuid.NewV7()),
		Environment: "production",
		ActorID:     "operator",
		RequestID:   uuid.Must(uuid.NewV7()).String(),
		Reason:      "provision a bound node",
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("CreateToken without endpoint error = %v, want ErrInvalidRequest", err)
	}
}

func TestEnrollmentProofV1GoldenAndBindings(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	request := enrollmentGoldenRequest(privateKey.Public().(ed25519.PublicKey))
	canonical, err := EnrollmentCanonicalV1(request)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, canonical)
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "enrollment-proof-v1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(fixture))
	if len(lines) != 2 {
		t.Fatalf("golden fixture fields=%d", len(lines))
	}
	if actual := hex.EncodeToString(canonical); actual != lines[0] {
		t.Fatalf("canonical mismatch\nactual=%s", actual)
	}
	if actual := hex.EncodeToString(signature); actual != lines[1] {
		t.Fatalf("signature mismatch\nactual=%s", actual)
	}
	request.Proof = &agentv1.EnrollmentProofV1{Version: EnrollmentProofVersionV1, Signature: signature}
	if err := verifyEnrollmentProof(request); err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(*agentv1.EnrollRequest)
	}{
		{"endpoint", func(value *agentv1.EnrollRequest) { value.EndpointId[0] ^= 1 }},
		{"token", func(value *agentv1.EnrollRequest) { value.Token += "x" }},
		{"capability", func(value *agentv1.EnrollRequest) { value.Capabilities[0] += ".changed" }},
		{"signature", func(value *agentv1.EnrollRequest) { value.Proof.Signature[0] ^= 1 }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			copyRequest := proto.Clone(request).(*agentv1.EnrollRequest)
			test.mutate(copyRequest)
			if err := verifyEnrollmentProof(copyRequest); err == nil {
				t.Fatal("mutated enrollment proof accepted")
			}
		})
	}
}

func enrollmentGoldenRequest(endpoint []byte) *agentv1.EnrollRequest {
	instanceID, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	nonce, _ := hex.DecodeString("ffeeddccbbaa99887766554433221100")
	return &agentv1.EnrollRequest{
		Token:                   "enrollment-token-fixture",
		EndpointId:              bytes.Clone(endpoint),
		AgentVersion:            "agent-1.2.3",
		OsRelease:               "FixtureOS 9",
		OcservVersion:           "1.3.0",
		BootId:                  "boot-fixture",
		AgentInstanceId:         instanceID,
		Capabilities:            []string{"ocserv.users.write", "ocserv.status.read"},
		Environment:             "production",
		Nonce:                   nonce,
		Time:                    &timestamppb.Timestamp{Seconds: 1_700_000_000, Nanos: 123_456_789},
		EnrollmentProtocolMajor: EnrollmentProtocolMajor,
		EnrollmentProtocolMinor: EnrollmentProtocolMinor,
	}
}
