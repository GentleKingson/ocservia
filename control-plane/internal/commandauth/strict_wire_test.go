package commandauth

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// These Go-encoded frames are also decoded by the Rust production strict decoder.
func TestStrictWireCommandFixtures(t *testing.T) {
	data, err := os.ReadFile("../../../testdata/command-strict-wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]string
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	secret := &agentv1.SealedSecretV1{Version: 1, Purpose: 1, KeyId: "test-key", Ciphertext: []byte("sealed-test-data")}
	p12Secret := &agentv1.SealedSecretV1{Version: 1, Purpose: 2, KeyId: "test-key", Ciphertext: []byte("sealed-test-data")}
	commands := map[string]*agentv1.CommandEnvelope{
		"user_create":          {Payload: &agentv1.CommandEnvelope_UserCreate{UserCreate: &agentv1.UserCreate{Username: "alice", DesiredRevision: 42, SealedPasswordV1: secret}}},
		"password_rotate":      {Payload: &agentv1.CommandEnvelope_UserPasswordRotate{UserPasswordRotate: &agentv1.UserPasswordRotate{Username: "alice", DesiredRevision: 42, SealedPasswordV1: secret}}},
		"config_plan_zero":     {Payload: &agentv1.CommandEnvelope_ConfigPlan{ConfigPlan: &agentv1.ConfigPlan{Candidate: []byte("config"), CandidateHash: []byte("hash")}}},
		"config_plan_revision": {Payload: &agentv1.CommandEnvelope_ConfigPlan{ConfigPlan: &agentv1.ConfigPlan{Candidate: []byte("config"), CandidateHash: []byte("hash"), ExpectedRevision: 42}}},
		"certificate_p12":      {Payload: &agentv1.CommandEnvelope_CertificateP12{CertificateP12: &agentv1.CertificateP12{CertificateId: []byte("certificate"), ArtifactId: []byte("artifact"), SealedPasswordV1: p12Secret, CertificateVersion: 42, ArtifactExpiresAt: &timestamppb.Timestamp{Seconds: 1700000060, Nanos: 123}}}},
		"certificate_revoke":   {Payload: &agentv1.CommandEnvelope_CertificateRevoke{CertificateRevoke: &agentv1.CertificateRevoke{CertificateId: []byte("certificate"), Reason: "rotation", CertificateVersion: 42}}},
	}
	if len(fixtures) != len(commands) {
		t.Errorf("fixture count = %d, want %d", len(fixtures), len(commands))
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			command.ProtocolVersion = "1.1"
			wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
			if err != nil {
				t.Fatal(err)
			}
			if actual := hex.EncodeToString(wire); actual != fixtures[name] {
				t.Errorf("Go wire fixture %s: got %s, want %s", name, actual, fixtures[name])
			}
		})
	}
}
