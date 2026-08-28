package operations

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func testCommandSigner(t *testing.T) *commandauth.Signer {
	t.Helper()
	var seed [32]byte
	seed[0] = 1
	return commandauth.NewSignerFromSeed(seed)
}

func TestCommandResultResponseTimeoutLeavesCompletionHeadroom(t *testing.T) {
	const completionTarget = 30 * time.Second
	const minimumObservationWindow = 20 * time.Second
	if commandResultResponseTimeout < minimumObservationWindow || commandResultResponseTimeout > completionTarget-5*time.Second {
		t.Fatalf("command result response timeout %s leaves insufficient recovery headroom below %s", commandResultResponseTimeout, completionTarget)
	}
}

func TestValidateCreateRejectsUntypedOrOversizedSyntheticPayload(t *testing.T) {
	base := CreateRequest{NodeID: uuid.Must(uuid.NewV7()), IdempotencyKey: "stable-key", ExpectedVersion: 1, Kind: SyntheticNoop, TTL: time.Minute, RequestID: "request", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"}
	for name, mutate := range map[string]func(*CreateRequest){
		"unknown kind":   func(r *CreateRequest) { r.Kind = "method.string" },
		"noop body":      func(r *CreateRequest) { r.Message = "not allowed" },
		"oversized echo": func(r *CreateRequest) { r.Kind = SyntheticEcho; r.Message = string(make([]byte, 4097)) },
		"long ttl":       func(r *CreateRequest) { r.TTL = 24*time.Hour + time.Second },
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
	request := CreateRequest{NodeID: uuid.Must(uuid.NewV7()), IdempotencyKey: "stable-key", ExpectedVersion: 4, Kind: SyntheticEcho, Message: "hello", TTL: time.Minute, RequestID: "request", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"}
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
			r.Kind = SessionDisconnect
			r.SessionID = "42"
			r.BootID = bootID
			return r
		}(),
		"terminate": func() CreateRequest {
			r := base
			r.Kind = SessionTerminate
			r.SessionID = "42"
			r.BootID = bootID
			r.Action = "session.terminate"
			return r
		}(),
		"unban": func() CreateRequest {
			r := base
			r.Kind = IPBanRemove
			r.IP = "192.0.2.9"
			r.Action = "ip_ban.remove"
			return r
		}(),
		"reload": func() CreateRequest {
			r := base
			r.Kind = ServiceReload
			r.Action = "service.reload"
			r.ActorIdentityID = uuid.Must(uuid.NewV7())
			r.ActorSessionID = uuid.Must(uuid.NewV7())
			r.ApprovalID = uuid.Must(uuid.NewV7())
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
	invalid.Kind = SessionDisconnect
	invalid.SessionID = "alice"
	invalid.BootID = bootID
	if err := validateCreate(invalid); err == nil {
		t.Fatal("username-like session target was accepted")
	}
	invalid.SessionID = "042"
	if err := validateCreate(invalid); err == nil {
		t.Fatal("non-canonical session target was accepted")
	}
	invalid = base
	invalid.Kind = IPBanRemove
	invalid.IP = "192.0.2.009"
	if err := validateCreate(invalid); err == nil {
		t.Fatal("non-canonical IP was accepted")
	}
}

func TestServiceReloadRequiresIndependentApprovalMetadata(t *testing.T) {
	request := CreateRequest{NodeID: uuid.Must(uuid.NewV7()), IdempotencyKey: "stable-key", ExpectedVersion: 1, Kind: ServiceReload, ActorID: "operator", Action: "service.reload", Reason: "change", TTL: time.Minute, RequestID: "request", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"}
	if err := validateCreate(request); err == nil {
		t.Fatal("high-risk reload accepted without approval metadata")
	}
	request.ActorIdentityID, request.ActorSessionID, request.ApprovalID = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := validateCreate(request); err != nil {
		t.Fatalf("approved reload rejected: %v", err)
	}
}

func TestRequestHashBindsTargetActorAndActionButNotAttemptMetadata(t *testing.T) {
	base := CreateRequest{NodeID: uuid.Must(uuid.NewV7()), IdempotencyKey: "stable-key", ExpectedVersion: 4, Kind: ServiceReload, ActorID: "operator", Action: "service.reload", Reason: "support case", TTL: time.Minute, RequestID: "request-one", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"}
	baseHash := requestHash(base)
	for name, mutate := range map[string]func(*CreateRequest){
		"node":          func(r *CreateRequest) { r.NodeID = uuid.Must(uuid.NewV7()) },
		"actor":         func(r *CreateRequest) { r.ActorID = "other-operator" },
		"action":        func(r *CreateRequest) { r.Action = "other.action" },
		"hold dispatch": func(r *CreateRequest) { r.HoldDispatch = true },
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
	return CreateRequest{NodeID: uuid.Must(uuid.NewV7()), IdempotencyKey: "stable-key", ExpectedVersion: 1, Kind: AgentUpgrade, ActorID: "operator", Action: "agent.upgrade", Reason: "roll out 1.2.3", TargetVersion: "1.2.3", PackageSHA256: make([]byte, 32), Architecture: "arm64", ActorIdentityID: uuid.Must(uuid.NewV7()), ActorSessionID: uuid.Must(uuid.NewV7()), ApprovalID: uuid.Must(uuid.NewV7()), TTL: time.Minute, RequestID: "request", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"}
}

func TestAgentUpgradeRequiresApprovalAndValidReleaseIdentity(t *testing.T) {
	base := agentUpgradeRequest()
	if err := validateCreate(base); err != nil {
		t.Fatalf("approved upgrade rejected: %v", err)
	}
	unapproved := base
	unapproved.ApprovalID = uuid.Nil
	if err := validateCreate(unapproved); err == nil {
		t.Fatal("upgrade accepted without approval metadata")
	}
	unidentified := base
	unidentified.ActorIdentityID = uuid.Nil
	if err := validateCreate(unidentified); err == nil {
		t.Fatal("upgrade accepted without actor identity")
	}
	for name, mutate := range map[string]func(*CreateRequest){
		"non-semver version": func(r *CreateRequest) { r.TargetVersion = "latest" },
		"partial version":    func(r *CreateRequest) { r.TargetVersion = "1.2" },
		"v-prefixed version": func(r *CreateRequest) { r.TargetVersion = "v1.2.3" },
		"short digest":       func(r *CreateRequest) { r.PackageSHA256 = make([]byte, 31) },
		"foreign arch":       func(r *CreateRequest) { r.Architecture = "x86_64" },
		"unknown arch":       func(r *CreateRequest) { r.Architecture = "riscv64" },
	} {
		t.Run(name, func(t *testing.T) {
			request := base
			mutate(&request)
			if err := validateCreate(request); err == nil {
				t.Fatal("malformed upgrade release identity was accepted")
			}
		})
	}
	leak := base
	leak.Kind = SyntheticEcho
	if err := validateCreate(leak); err == nil {
		t.Fatal("upgrade release fields leaked into another kind")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*CreateRequest)
	}{
		{"candidate", func(r *CreateRequest) { r.Candidate = []byte("config") }},
		{"candidate hash", func(r *CreateRequest) { r.CandidateHash = make([]byte, sha256.Size) }},
		{"expected current hash", func(r *CreateRequest) { r.ExpectedCurrentHash = make([]byte, sha256.Size) }},
		{"desired revision", func(r *CreateRequest) { r.DesiredRevision = 7 }},
		{"plan revision", func(r *CreateRequest) { r.PlanRevision = 3 }},
		{"plan metadata", func(r *CreateRequest) { r.PlanMetadata = &ConfigPlanMetadata{} }},
		{"apply metadata", func(r *CreateRequest) { r.ApplyMetadata = &ConfigApplyMetadata{} }},
		{"ocserv version", func(r *CreateRequest) { r.OcservVersion = "1.2.3" }},
		{"plan capabilities", func(r *CreateRequest) { r.PlanCapabilities = []string{"ocserv.config.capabilities.v1"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := base
			tc.mutate(&request)
			if err := validateCreate(request); err == nil {
				t.Fatalf("config field %q was silently dropped from an upgrade request", tc.name)
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
	if upgrade == nil || upgrade.GetTargetVersion() != request.TargetVersion || !bytes.Equal(upgrade.GetPackageSha256(), request.PackageSHA256) || upgrade.GetArchitecture() != request.Architecture {
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
		mutate func(*CreateRequest)
	}{
		{"target version", func(r *CreateRequest) { r.TargetVersion = "2.0.0" }},
		{"package digest", func(r *CreateRequest) { r.PackageSHA256[0] ^= 0xff }},
		{"architecture", func(r *CreateRequest) { r.Architecture = "amd64" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := base
			request.PackageSHA256 = append([]byte(nil), base.PackageSHA256...)
			tc.mutate(&request)
			if requestHash(request) == baseHash {
				t.Fatalf("%s was not bound into the idempotency digest", tc.name)
			}
		})
	}
}
