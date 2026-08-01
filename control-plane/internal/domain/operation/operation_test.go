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
			op := Operation{ID: uuid.New(), WorkspaceID: uuid.New(), State: state, Version: 1, CreatedAt: now, UpdatedAt: now}
			if err := op.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}
