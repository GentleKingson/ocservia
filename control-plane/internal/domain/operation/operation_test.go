package operation

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestValidateAcceptsContractedStates(t *testing.T) {
	states := []State{
		StateDraft, StateQueued, StateDispatched, StateAccepted, StateRunning,
		StateSucceeded, StateFailed, StateUnknown, StateExpired, StateRolledBack,
		StateOfflinePending, StateDrifted, StateSuperseded,
	}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			now := time.Now()
			op := Operation{ID: newV7(t), WorkspaceID: newV7(t), State: state, Version: 1, CreatedAt: now, UpdatedAt: now}
			if err := op.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestValidateRejectsNonV7Identifiers(t *testing.T) {
	op := Operation{ID: uuid.New(), WorkspaceID: newV7(t), State: StateDraft, Version: 1}
	if err := op.Validate(); err == nil {
		t.Fatal("Validate() accepted a non-v7 operation ID")
	}
}

func newV7(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("NewV7() error = %v", err)
	}
	return id
}
