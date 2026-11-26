package operations

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const testTraceparent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"

func testCommandSigner() *commandauth.Signer {
	return commandauth.NewSignerFromSeed([32]byte{1})
}

func requestHash(r CreateRequest) [32]byte { return requestDigest(r).Current }

func baseCreateRequest(payload Payload, action string) CreateRequest {
	return CreateRequest{
		NodeID:          uuid.Must(uuid.NewV7()),
		IdempotencyKey:  "stable-key",
		ExpectedVersion: 1,
		ActorID:         "operator",
		ActorIdentityID: uuid.Must(uuid.NewV7()),
		ActorSessionID:  uuid.Must(uuid.NewV7()),
		Action:          action,
		Reason:          "support case",
		TTL:             time.Minute,
		RequestID:       "request",
		Traceparent:     testTraceparent,
		Payload:         payload,
	}
}

func TestValidateCreateAndActorSessionGuards(t *testing.T) {
	t.Parallel()
	bootID := uuid.NewString()
	for _, tc := range []struct {
		name string
		req  CreateRequest
		ok   bool
	}{
		{"echo", baseCreateRequest(SyntheticEchoPayload{Message: "hello"}, "operation.create"), true},
		{"missing payload", baseCreateRequest(nil, "operation.create"), false},
		{"oversized echo", baseCreateRequest(SyntheticEchoPayload{Message: string(make([]byte, 4097))}, "operation.create"), false},
		{"disconnect", baseCreateRequest(SessionDisconnectPayload{SessionID: "42", BootID: bootID}, "session.disconnect"), true},
		{"bad session id", baseCreateRequest(SessionDisconnectPayload{SessionID: "042", BootID: bootID}, "session.disconnect"), false},
		{"unban", baseCreateRequest(IPBanRemovePayload{IP: "192.0.2.9"}, "ip_ban.remove"), true},
		{"non-canonical ip", baseCreateRequest(IPBanRemovePayload{IP: "192.0.2.009"}, "ip_ban.remove"), false},
		{"reload", baseCreateRequest(ServiceReloadPayload{}, "service.reload"), true},
		{"reload missing session", func() CreateRequest {
			r := baseCreateRequest(ServiceReloadPayload{}, "service.reload")
			r.ActorSessionID = uuid.Nil
			return r
		}(), false},
		{"upgrade valid", baseCreateRequest(AgentUpgradePayload{TargetVersion: "1.2.3", PackageSHA256: make([]byte, 32), Architecture: "arm64"}, "agent.upgrade"), true},
		{"upgrade bad version", baseCreateRequest(AgentUpgradePayload{TargetVersion: "v1.2.3", PackageSHA256: make([]byte, 32), Architecture: "arm64"}, "agent.upgrade"), false},
		{"upgrade bad arch", baseCreateRequest(AgentUpgradePayload{TargetVersion: "1.2.3", PackageSHA256: make([]byte, 32), Architecture: "x86_64"}, "agent.upgrade"), false},
		{"upgrade non-v7 rollout", baseCreateRequest(AgentUpgradePayload{TargetVersion: "1.2.3", PackageSHA256: make([]byte, 32), Architecture: "arm64", RolloutID: uuid.New()}, "agent.upgrade"), false},
	} {
		err := validateCreate(tc.req)
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err=%v want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestMarshalEnvelopeAndSemanticSigning(t *testing.T) {
	t.Parallel()
	signer := testCommandSigner()
	req := baseCreateRequest(AgentUpgradePayload{TargetVersion: "1.2.3", PackageSHA256: make([]byte, 32), Architecture: "arm64"}, "agent.upgrade")
	data, payloadType, err := marshalEnvelope(req, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), 1, time.Now(), time.Now().Add(time.Minute), signer)
	if err != nil || payloadType != "agent_upgrade" || len(data) == 0 {
		t.Fatalf("marshalEnvelope() = %q, %d bytes, %v", payloadType, len(data), err)
	}
	var env agentv1.CommandEnvelope
	if err := proto.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	claims, err := commandauth.ClaimsFromEnvelopeV1(&env)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := commandauth.CanonicalV1(claims)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := semanticpayload.HashV2(&env)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(signer.PublicKey(), canonical, env.GetAuthorization().GetSignature()) || !bytes.Equal(claims.SemanticPayloadSHA256[:], digest[:]) {
		t.Fatal("command authorization or semantic hash mismatch")
	}
	mutated := proto.Clone(&env).(*agentv1.CommandEnvelope)
	mutated.GetAgentUpgrade().TargetVersion = "9.9.9"
	if mutatedDigest, err := semanticpayload.HashV2(mutated); err != nil || bytes.Equal(mutatedDigest[:], digest[:]) {
		t.Fatal("mutated target version matched original semantic hash")
	}
}

func TestRequestHashBindsEffectNotAttemptMetadata(t *testing.T) {
	t.Parallel()
	base := baseCreateRequest(ServiceReloadPayload{}, "service.reload")
	baseHash := requestHash(base)
	attempt := base
	attempt.RequestID = "other-request"
	attempt.Traceparent = "00-1123456789abcdef0123456789abcdef-1123456789abcdef-01"
	if requestHash(attempt) != baseHash {
		t.Fatal("attempt metadata changed idempotency hash")
	}
	changed := base
	changed.ActorID = "other-operator"
	if requestHash(changed) == baseHash {
		t.Fatal("actor was not bound into idempotency hash")
	}
	if !errors.Is(validateCreate(base), nil) {
		t.Fatal("valid request failed")
	}
}
