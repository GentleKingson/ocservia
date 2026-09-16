package configplan

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
)

type recordingOperationCreator struct {
	delegate operationCreator
	contexts []context.Context
	requests []operations.CreateRequest
	err      error
}

func (r *recordingOperationCreator) CreateSynthetic(ctx context.Context, request operations.CreateRequest) (operations.Operation, bool, error) {
	r.contexts = append(r.contexts, ctx)
	r.requests = append(r.requests, request)
	if r.err != nil {
		return operations.Operation{}, false, r.err
	}
	return r.delegate.CreateSynthetic(ctx, request)
}

func (r *recordingOperationCreator) assertLast(t *testing.T, ctx context.Context, count int, want operations.CreateRequest) {
	t.Helper()
	if len(r.requests) != count {
		t.Fatalf("CreateSynthetic calls = %d, want %d", len(r.requests), count)
	}
	if r.contexts[count-1] != ctx || !reflect.DeepEqual(r.requests[count-1], want) {
		t.Fatalf("CreateSynthetic context preserved = %v, request = %+v, want %+v", r.contexts[count-1] == ctx, r.requests[count-1], want)
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
	for _, pair := range [][2]error{{ErrStaleRevision, operations.ErrStaleRevision}, {ErrIdempotency, operations.ErrIdempotencyConflict}} {
		if pair[0] != pair[1] || !errors.Is(fmt.Errorf("downstream: %w", pair[0]), pair[1]) {
			t.Fatal("Operations error identity changed")
		}
	}
}
