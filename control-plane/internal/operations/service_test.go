package operations

import (
	"crypto/ed25519"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
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
