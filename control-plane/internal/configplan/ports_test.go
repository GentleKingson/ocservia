package configplan

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"google.golang.org/protobuf/proto"
)

type recordingOperationCreator struct {
	delegate operationCreator
	contexts []context.Context
	requests []operations.CreateRequest
	err      error
}

func (r *recordingOperationCreator) Create(ctx context.Context, request operations.CreateRequest) (operations.Operation, bool, error) {
	r.contexts = append(r.contexts, ctx)
	r.requests = append(r.requests, request)
	if r.err != nil {
		return operations.Operation{}, false, r.err
	}
	return r.delegate.Create(ctx, request)
}

func (r *recordingOperationCreator) assertLast(t *testing.T, ctx context.Context, count int, want operations.CreateRequest) {
	t.Helper()
	if len(r.requests) != count {
		t.Fatalf("Create calls = %d, want %d", len(r.requests), count)
	}
	got := r.requests[count-1]
	switch payload := got.Payload.(type) {
	case operations.ConfigPlanPayload:
		expected, ok := want.Payload.(operations.ConfigPlanPayload)
		if !ok || !proto.Equal(payload.CompleteCandidate, expected.CompleteCandidate) {
			t.Fatal("Create plan proto fields mismatch")
		}
		payload.CompleteCandidate, expected.CompleteCandidate = nil, nil
		got.Payload, want.Payload = payload, expected
	case operations.ConfigApplyPayload:
		expected, ok := want.Payload.(operations.ConfigApplyPayload)
		if !ok || !proto.Equal(payload.CompleteCandidate, expected.CompleteCandidate) || !proto.Equal(payload.PlanExpiresAt, expected.PlanExpiresAt) {
			t.Fatal("Create apply proto fields mismatch")
		}
		payload.CompleteCandidate, expected.CompleteCandidate = nil, nil
		payload.PlanExpiresAt, expected.PlanExpiresAt = nil, nil
		got.Payload, want.Payload = payload, expected
	}
	if r.contexts[count-1] != ctx || !reflect.DeepEqual(got, want) {
		t.Fatalf("Create context preserved = %v, request = %+v, want %+v", r.contexts[count-1] == ctx, got, want)
	}
}

func TestOperationCreatorValidationAndErrors(t *testing.T) {
	ctx := t.Context()
	recorder := &recordingOperationCreator{}
	var typedNil *operations.Service
	for _, dependency := range []operationCreator{nil, typedNil, recorder} {
		s := NewBackend(nil, dependency)
		if _, replay, err := s.Create(ctx, CreateRequest{}); !errors.Is(err, ErrInvalid) || replay {
			t.Fatalf("invalid Create: replay=%v err=%v", replay, err)
		}
		if _, replay, err := s.Apply(ctx, ApplyRequest{}); !errors.Is(err, ErrInvalid) || replay {
			t.Fatalf("invalid Apply: replay=%v err=%v", replay, err)
		}
	}
	if len(recorder.requests) != 0 {
		t.Fatal("invalid request reached operation creator")
	}
	if ErrStaleRevision != operations.ErrStaleRevision || !errors.Is(fmt.Errorf("downstream: %w", ErrStaleRevision), operations.ErrStaleRevision) {
		t.Fatal("Operations error identity changed")
	}
}
