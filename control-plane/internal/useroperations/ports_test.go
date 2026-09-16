package useroperations

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
)

type recordingUserMutator struct {
	delegate userMutator
	contexts []context.Context
	requests []userstate.MutationRequest
	err      error
}

func (r *recordingUserMutator) Mutate(ctx context.Context, request userstate.MutationRequest) (operations.Operation, bool, error) {
	r.contexts = append(r.contexts, ctx)
	r.requests = append(r.requests, request)
	if r.err != nil {
		return operations.Operation{}, false, r.err
	}
	return r.delegate.Mutate(ctx, request)
}

func (r *recordingUserMutator) assertLast(t *testing.T, ctx context.Context, count int, want userstate.MutationRequest) {
	t.Helper()
	if len(r.requests) != count {
		t.Fatalf("Mutate calls = %d, want %d", len(r.requests), count)
	}
	if r.contexts[count-1] != ctx || !reflect.DeepEqual(r.requests[count-1], want) {
		t.Fatalf("Mutate context preserved = %v, request = %+v, want %+v", r.contexts[count-1] == ctx, r.requests[count-1], want)
	}
}

func TestUserMutatorValidationAndConcurrency(t *testing.T) {
	recorder := &recordingUserMutator{}
	var typedNil *userstate.Service
	for _, dependency := range []userMutator{nil, typedNil, recorder} {
		for _, limit := range []int{-1, 0, 3} {
			s := NewWithConcurrencyBackend(nil, dependency, limit)
			want := DefaultGlobalConcurrency
			if limit > 0 {
				want = limit
			}
			if s.batchSize != want {
				t.Fatalf("concurrency = %d, want %d", s.batchSize, want)
			}
			if _, _, err := s.SetPolicy(t.Context(), PolicyRequest{}); !errors.Is(err, ErrInvalidRequest) {
				t.Fatal("invalid policy", err)
			}
			if _, _, err := s.CreateBatch(t.Context(), BatchRequest{}); !errors.Is(err, ErrInvalidRequest) {
				t.Fatal("invalid batch", err)
			}
			if n, err := s.resetMonthlyPolicies(t.Context(), 0); n != 0 || err != nil {
				t.Fatal("zero reset budget", n, err)
			}
			if n, err := s.enforcePolicies(t.Context(), 0); n != 0 || err != nil {
				t.Fatal("zero enforcement budget", n, err)
			}
		}
	}
	if len(recorder.requests) != 0 {
		t.Fatal("rejected request reached user mutator")
	}
}
