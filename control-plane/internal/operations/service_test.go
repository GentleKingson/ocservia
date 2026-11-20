package operations

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const testTraceparent = testfixture.Traceparent

func testCommandSigner(t *testing.T) *commandauth.Signer {
	t.Helper()
	return testfixture.CommandSigner(1)
}

func TestCommandResultResponseTimeoutLeavesCompletionHeadroom(t *testing.T) {
	const completionTarget = 30 * time.Second
	const minimumObservationWindow = 20 * time.Second
	if commandResultResponseTimeout < minimumObservationWindow || commandResultResponseTimeout > completionTarget-5*time.Second {
		t.Fatalf("command result response timeout %s leaves insufficient recovery headroom below %s", commandResultResponseTimeout, completionTarget)
	}
}

func TestValidateCreateRejectsUntypedOrOversizedSyntheticPayload(t *testing.T) {
	base := CreateRequest{NodeID: uuid.Must(uuid.NewV7()), IdempotencyKey: "stable-key", ExpectedVersion: 1, TTL: time.Minute, RequestID: "request", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Payload: SyntheticNoopPayload{}}
	for name, mutate := range map[string]func(*CreateRequest){
		"missing payload": func(r *CreateRequest) { r.Payload = nil },
		"pointer payload": func(r *CreateRequest) { r.Payload = (*SyntheticNoopPayload)(nil) },
		"oversized echo":  func(r *CreateRequest) { r.Payload = SyntheticEchoPayload{Message: string(make([]byte, 4097))} },
		"long ttl":        func(r *CreateRequest) { r.TTL = 24*time.Hour + time.Second },
	} {
		t.Run(name, func(t *testing.T) {
			request := base
			mutate(&request)
			if err := validateCreate(request); err == nil {
				t.Fatal("invalid request was accepted")
			}
		})
	}
}

func TestMarshalEnvelopeUsesTypedOneof(t *testing.T) {
	request := CreateRequest{NodeID: uuid.Must(uuid.NewV7()), IdempotencyKey: "stable-key", ExpectedVersion: 4, TTL: time.Minute, RequestID: "request", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Payload: SyntheticEchoPayload{Message: "hello"}}
	signer := testCommandSigner(t)
	data, payloadType, err := marshalEnvelope(request, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), 1, time.Now(), time.Now().Add(time.Minute), signer)
	if err != nil || payloadType != "synthetic_echo" || len(data) == 0 {
		t.Fatalf("marshalEnvelope() = %q, %d bytes, %v", payloadType, len(data), err)
	}
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	claims, err := commandauth.ClaimsFromEnvelopeV1(&envelope)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := commandauth.CanonicalV1(claims)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.GetProtocolVersion() != commandauth.ProtocolVersion || envelope.GetSemanticPayloadHashVersion() != agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2 || envelope.GetAction() != "operation.create" || envelope.GetRequiredCapability() != "synthetic.echo" || !ed25519.Verify(signer.PublicKey(), canonical, envelope.GetAuthorization().GetSignature()) {
		t.Fatal("typed command was not authorized after its final semantic fields")
	}
}

func TestControlledOperationsRequireTypedTargetsAndReason(t *testing.T) {
	base := CreateRequest{NodeID: uuid.Must(uuid.NewV7()), IdempotencyKey: "stable-key", ExpectedVersion: 4, TTL: time.Minute, RequestID: "request", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", ActorID: "operator", Reason: "support case", Action: "session.disconnect"}
	bootID := uuid.NewString()
	for name, request := range map[string]CreateRequest{
		"disconnect": func() CreateRequest {
			r := base
			r.Payload = SessionDisconnectPayload{SessionID: "42", BootID: bootID}
			return r
		}(),
		"terminate": func() CreateRequest {
			r := base
			r.Payload = SessionTerminatePayload{SessionID: "42", BootID: bootID}
			r.Action = "session.terminate"
			return r
		}(),
		"unban": func() CreateRequest {
			r := base
			r.Payload = IPBanRemovePayload{IP: "192.0.2.9"}
			r.Action = "ip_ban.remove"
			return r
		}(),
		"reload": func() CreateRequest {
			r := base
			r.Payload = ServiceReloadPayload{}
			r.Action = "service.reload"
			r.ActorIdentityID = uuid.Must(uuid.NewV7())
			r.ActorSessionID = uuid.Must(uuid.NewV7())
			return r
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCreate(request); err != nil {
				t.Fatalf("validateCreate() = %v", err)
			}
			data, payloadType, err := marshalEnvelope(request, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), 1, time.Now(), time.Now().Add(time.Minute), testCommandSigner(t))
			if err != nil || len(data) == 0 || payloadType == "" {
				t.Fatalf("marshalEnvelope() = %q, %d, %v", payloadType, len(data), err)
			}
		})
	}
	invalid := base
	invalid.Payload = SessionDisconnectPayload{SessionID: "alice", BootID: bootID}
	if err := validateCreate(invalid); err == nil {
		t.Fatal("username-like session target was accepted")
	}
	invalid.Payload = SessionDisconnectPayload{SessionID: "042", BootID: bootID}
	if err := validateCreate(invalid); err == nil {
		t.Fatal("non-canonical session target was accepted")
	}
	invalid = base
	invalid.Payload = IPBanRemovePayload{IP: "192.0.2.009"}
	if err := validateCreate(invalid); err == nil {
		t.Fatal("non-canonical IP was accepted")
	}
}

func TestServiceReloadRequiresActorSession(t *testing.T) {
	request := CreateRequest{NodeID: uuid.Must(uuid.NewV7()), IdempotencyKey: "stable-key", ExpectedVersion: 1, ActorID: "operator", Action: "service.reload", Reason: "change", TTL: time.Minute, RequestID: "request", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Payload: ServiceReloadPayload{}}
	if err := validateCreate(request); err == nil {
		t.Fatal("high-risk reload accepted without actor session")
	}
	request.ActorIdentityID, request.ActorSessionID = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := validateCreate(request); err != nil {
		t.Fatalf("authorized reload rejected: %v", err)
	}
}

func TestRequestHashBindsTargetActorAndActionButNotAttemptMetadata(t *testing.T) {
	base := CreateRequest{NodeID: uuid.Must(uuid.NewV7()), IdempotencyKey: "stable-key", ExpectedVersion: 4, ActorID: "operator", Action: "service.reload", Reason: "support case", TTL: time.Minute, RequestID: "request-one", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Payload: ServiceReloadPayload{}}
	baseHash := requestHash(base)
	for name, mutate := range map[string]func(*CreateRequest){
		"node":    func(r *CreateRequest) { r.NodeID = uuid.Must(uuid.NewV7()) },
		"actor":   func(r *CreateRequest) { r.ActorID = "other-operator" },
		"action":  func(r *CreateRequest) { r.Action = "other.action" },
		"payload": func(r *CreateRequest) { r.Payload = CertificateRevokePayload{HoldDispatch: true} },
	} {
		t.Run(name, func(t *testing.T) {
			request := base
			mutate(&request)
			if requestHash(request) == baseHash {
				t.Fatalf("%s was not bound into the idempotency digest", name)
			}
		})
	}
	attempt := base
	attempt.RequestID = "request-two"
	attempt.Traceparent = "00-1123456789abcdef0123456789abcdef-1123456789abcdef-01"
	if requestHash(attempt) != baseHash {
		t.Fatal("attempt-only metadata changed the idempotency digest")
	}
}

func agentUpgradeRequest() CreateRequest {
	return CreateRequest{
		NodeID:          uuid.Must(uuid.NewV7()),
		IdempotencyKey:  "stable-key",
		ExpectedVersion: 1,
		ActorID:         "operator",
		Action:          "agent.upgrade",
		Reason:          "roll out 1.2.3",
		ActorIdentityID: uuid.Must(uuid.NewV7()),
		ActorSessionID:  uuid.Must(uuid.NewV7()),
		TTL:             time.Minute,
		RequestID:       "request",
		Traceparent:     "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
		Payload: AgentUpgradePayload{
			TargetVersion: "1.2.3",
			PackageSHA256: make([]byte, 32),
			Architecture:  "arm64",
		},
	}
}

func TestAgentUpgradeRequiresActorAndValidReleaseIdentity(t *testing.T) {
	base := agentUpgradeRequest()
	if err := validateCreate(base); err != nil {
		t.Fatalf("single administrator upgrade rejected: %v", err)
	}
	unidentified := base
	unidentified.ActorIdentityID = uuid.Nil
	if err := validateCreate(unidentified); err == nil {
		t.Fatal("upgrade accepted without actor identity")
	}
	for name, mutate := range map[string]func(*AgentUpgradePayload){
		"non-semver version": func(r *AgentUpgradePayload) { r.TargetVersion = "latest" },
		"partial version":    func(r *AgentUpgradePayload) { r.TargetVersion = "1.2" },
		"v-prefixed version": func(r *AgentUpgradePayload) { r.TargetVersion = "v1.2.3" },
		"short digest":       func(r *AgentUpgradePayload) { r.PackageSHA256 = make([]byte, 31) },
		"foreign arch":       func(r *AgentUpgradePayload) { r.Architecture = "x86_64" },
		"unknown arch":       func(r *AgentUpgradePayload) { r.Architecture = "riscv64" },
	} {
		t.Run(name, func(t *testing.T) {
			request := base
			payload := request.Payload.(AgentUpgradePayload)
			mutate(&payload)
			request.Payload = payload
			if err := validateCreate(request); err == nil {
				t.Fatal("malformed upgrade release identity was accepted")
			}
		})
	}
}

func TestMarshalEnvelopeSignsAgentUpgradeReleaseIdentity(t *testing.T) {
	request := agentUpgradeRequest()
	signer := testCommandSigner(t)
	data, payloadType, err := marshalEnvelope(request, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), 1, time.Now(), time.Now().Add(time.Minute), signer)
	if err != nil || payloadType != "agent_upgrade" || len(data) == 0 {
		t.Fatalf("marshalEnvelope() = %q, %d bytes, %v", payloadType, len(data), err)
	}
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.GetAction() != "agent.upgrade" || envelope.GetRequiredCapability() != "ocserv.agent.upgrade.v2" {
		t.Fatalf("upgrade authorization labels = %q/%q", envelope.GetAction(), envelope.GetRequiredCapability())
	}
	upgrade := envelope.GetAgentUpgrade()
	if upgrade == nil || upgrade.GetTargetVersion() != request.Payload.(AgentUpgradePayload).TargetVersion || !bytes.Equal(upgrade.GetPackageSha256(), request.Payload.(AgentUpgradePayload).PackageSHA256) || upgrade.GetArchitecture() != request.Payload.(AgentUpgradePayload).Architecture {
		t.Fatal("upgrade payload did not carry the requested release identity")
	}
	claims, err := commandauth.ClaimsFromEnvelopeV1(&envelope)
	if err != nil {
		t.Fatal(err)
	}
	if claims.PayloadKind != 128 || claims.Action != "agent.upgrade" || claims.RequiredCapability != "ocserv.agent.upgrade.v2" {
		t.Fatalf("signed claims did not bind the upgrade identity: %+v", claims)
	}
	digest, err := semanticpayload.HashV2(&envelope)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(claims.SemanticPayloadSHA256[:], digest[:]) {
		t.Fatal("signed semantic hash does not cover the upgrade release identity")
	}
	mutated := proto.Clone(&envelope).(*agentv1.CommandEnvelope)
	mutated.GetAgentUpgrade().TargetVersion = "9.9.9"
	mutatedDigest, err := semanticpayload.HashV2(mutated)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(mutatedDigest[:], claims.SemanticPayloadSHA256[:]) {
		t.Fatal("mutated target version still matches the signed semantic hash")
	}
	mutated.GetAgentUpgrade().PackageSha256[0] ^= 0xff
	digestAgain, err := semanticpayload.HashV2(mutated)
	if err != nil || bytes.Equal(digestAgain[:], mutatedDigest[:]) {
		t.Fatal("mutated package digest did not change the semantic hash")
	}
}

func TestRequestHashBindsAgentUpgradeReleaseIdentity(t *testing.T) {
	base := agentUpgradeRequest()
	baseHash := requestHash(base)
	for _, tc := range []struct {
		name   string
		mutate func(*AgentUpgradePayload)
	}{
		{"target version", func(r *AgentUpgradePayload) { r.TargetVersion = "2.0.0" }},
		{"package digest", func(r *AgentUpgradePayload) { r.PackageSHA256[0] ^= 0xff }},
		{"architecture", func(r *AgentUpgradePayload) { r.Architecture = "amd64" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := base
			payload := request.Payload.(AgentUpgradePayload)
			payload.PackageSHA256 = append([]byte(nil), payload.PackageSHA256...)
			tc.mutate(&payload)
			request.Payload = payload
			if requestHash(request) == baseHash {
				t.Fatalf("%s was not bound into the idempotency digest", tc.name)
			}
		})
	}
}

func TestConfigAndCertificatePayloadValidation(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	fixtureData, err := os.ReadFile("../../../testdata/complete-config-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]string
	if err := json.Unmarshal(fixtureData, &fixture); err != nil {
		t.Fatal(err)
	}
	encodedCandidate, err := hex.DecodeString(fixture["candidate_proto"])
	if err != nil {
		t.Fatal(err)
	}
	var complete agentv1.CompleteConfigCandidate
	if err := proto.Unmarshal(encodedCandidate, &complete); err != nil {
		t.Fatal(err)
	}
	complete.NodeId, complete.ExpectedRevision = id[:], 0
	canonical, err := configprofile.Canonical(&complete)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	expiry := timestamppb.New(time.Now().UTC().Add(time.Minute))
	plan := ConfigPlanPayload{CompleteCandidate: &complete, Candidate: canonical, CandidateHash: digest[:], PlanCapabilities: []string{configprofile.PlanCapability}, PlanMetadata: &ConfigPlanMetadata{TemplateName: "default", CandidateRedacted: "config"}}
	apply := ConfigApplyPayload{CompleteCandidate: &complete, Candidate: canonical, CandidateHash: digest[:], ExpectedCurrentHash: digest[:], DesiredRevision: 1, MaterializedHash: digest[:], PlanExpiresAt: expiry, ApplyMetadata: &ConfigApplyMetadata{PlanID: id}}
	csr := CertificateCSRPayload{CertificateID: id, CommonName: "node.example.test", DNSNames: []string{"node.example.test"}, KeyBits: 2048}
	p12 := CertificateP12Payload{CertificateID: id, CertificateVersion: 1, CertificateChain: bytes.Repeat([]byte{1}, 64), ArtifactID: id, ArtifactMetadata: &ArtifactMetadata{TokenSHA256: digest[:], RequestHash: digest[:], ExpiresAt: time.Now().UTC().Add(time.Minute)}, SealedPassword: &agentv1.SealedSecretV1{Version: agentv1.SealedSecretVersion_SEALED_SECRET_VERSION_V1, Purpose: agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD, KeyId: "secret", Ciphertext: bytes.Repeat([]byte{1}, 32)}}
	revoke := CertificateRevokePayload{CertificateID: id, CertificateVersion: 1, RevocationReason: "rotate", HoldDispatch: true}
	for _, tc := range []struct {
		action  string
		valid   Payload
		invalid Payload
	}{
		{"config.plan", plan, ConfigPlanPayload{Candidate: canonical, CandidateHash: digest[:], PlanCapabilities: plan.PlanCapabilities, PlanMetadata: plan.PlanMetadata}},
		{"config.apply", apply, ConfigApplyPayload{Candidate: canonical, CandidateHash: digest[:], ExpectedCurrentHash: digest[:], DesiredRevision: 1, ApplyMetadata: apply.ApplyMetadata}},
		{"config.apply", apply, ConfigApplyPayload{CompleteCandidate: &complete, Candidate: canonical, CandidateHash: digest[:], ExpectedCurrentHash: digest[:], DesiredRevision: 1, ApplyMetadata: apply.ApplyMetadata}},
		{"certificate.issue", csr, CertificateCSRPayload{CertificateID: id, CommonName: csr.CommonName, KeyBits: 1024}},
		{"certificate.private_key.export", p12, CertificateP12Payload{CertificateID: id, CertificateVersion: 1, CertificateChain: p12.CertificateChain, SealedPassword: p12.SealedPassword, ArtifactID: id}},
		{"certificate.revoke", revoke, CertificateRevokePayload{CertificateID: id, RevocationReason: "rotate"}},
	} {
		t.Run(tc.action, func(t *testing.T) {
			request := CreateRequest{NodeID: id, IdempotencyKey: "validation", ExpectedVersion: 1, Payload: tc.valid, ActorID: "operator", ActorIdentityID: id, ActorSessionID: id, Action: tc.action, Reason: "support", TTL: time.Minute, RequestID: "request", Traceparent: testTraceparent}
			if err := validateCreate(request); err != nil {
				t.Fatalf("valid payload: %v", err)
			}
			request.Payload = tc.invalid
			if err := validateCreate(request); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("invalid payload: %v", err)
			}
		})
	}
}

func TestRequestHashRetainsCertificateDeliveryAndGeneratedIDRules(t *testing.T) {
	request := CreateRequest{Payload: CertificateRevokePayload{CertificateID: uuid.Must(uuid.NewV7()), CertificateVersion: 1, RevocationReason: "rotate"}}
	original := requestHash(request)
	payload := request.Payload.(CertificateRevokePayload)
	payload.HoldDispatch = true
	request.Payload = payload
	if requestHash(request) == original {
		t.Fatal("hold dispatch was not bound")
	}
	csr := CertificateCSRPayload{CertificateID: uuid.Must(uuid.NewV7()), CommonName: "node.example.test", KeyBits: 2048}
	request.Payload = csr
	original = requestHash(request)
	csr.CertificateID = uuid.Must(uuid.NewV7())
	request.Payload = csr
	if requestHash(request) != original {
		t.Fatal("generated CSR ID changed replay identity")
	}
}

func TestUpgradeRolloutPayloadRequiresClaimBinding(t *testing.T) {
	request := agentUpgradeRequest()
	payload := request.Payload.(AgentUpgradePayload)
	payload.RolloutRequestHash = make([]byte, sha256.Size)
	request.Payload = payload
	if err := validateCreate(request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unclaimed rollout hash: %v", err)
	}
	payload.RolloutID = uuid.Must(uuid.NewV7())
	request.Payload = payload
	if err := validateCreate(request); err != nil {
		t.Fatalf("valid rollout binding: %v", err)
	}
	payload.RolloutRequestHash = nil
	request.Payload = payload
	if err := validateCreate(request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing rollout hash: %v", err)
	}
}
