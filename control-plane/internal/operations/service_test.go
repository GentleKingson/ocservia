package operations

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

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
	data, payloadType, err := marshalEnvelope(request, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), time.Now(), time.Now().Add(time.Minute))
	if err != nil || payloadType != "synthetic_echo" || len(data) == 0 {
		t.Fatalf("marshalEnvelope() = %q, %d bytes, %v", payloadType, len(data), err)
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
		"reload": func() CreateRequest { r := base; r.Kind = ServiceReload; r.Action = "service.reload"; return r }(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCreate(request); err != nil {
				t.Fatalf("validateCreate() = %v", err)
			}
			data, payloadType, err := marshalEnvelope(request, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), time.Now(), time.Now().Add(time.Minute))
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

func TestRequestHashBindsTargetActorAndActionButNotAttemptMetadata(t *testing.T) {
	base := CreateRequest{NodeID: uuid.Must(uuid.NewV7()), IdempotencyKey: "stable-key", ExpectedVersion: 4, Kind: ServiceReload, ActorID: "operator", Action: "service.reload", Reason: "support case", TTL: time.Minute, RequestID: "request-one", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"}
	baseHash := requestHash(base)
	for name, mutate := range map[string]func(*CreateRequest){
		"node":   func(r *CreateRequest) { r.NodeID = uuid.Must(uuid.NewV7()) },
		"actor":  func(r *CreateRequest) { r.ActorID = "other-operator" },
		"action": func(r *CreateRequest) { r.Action = "other.action" },
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
