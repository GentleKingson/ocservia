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
