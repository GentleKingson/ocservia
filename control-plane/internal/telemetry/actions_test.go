package telemetry

import (
	"context"
	"errors"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

type actionStore struct {
	store.Store
	approved map[string]bool
	err      error
}

func (s actionStore) HasCapability(_ context.Context, _ uuid.UUID, capability string) (bool, error) {
	return s.approved[capability], s.err
}
func TestNodeActionAvailability(t *testing.T) {
	for _, status := range []string{"active", "offline", "pending", "revoked"} {
		t.Run(status, func(t *testing.T) {
			actions, err := nodeActions(context.Background(), actionStore{approved: map[string]bool{"ocserv.users.write": true, "ocserv.config.complete.plan": true}}, uuid.New(), status)
			if err != nil {
				t.Fatal(err)
			}
			writable := status == "active" || status == "offline"
			if actions["user.manage"].Allowed != writable || actions["config.plan"].Allowed != writable || actions["group.manage"].Allowed {
				t.Fatalf("wrong capabilities: %+v", actions)
			}
			if !actions["certificate.read"].Allowed {
				t.Fatal("read must not require a write capability")
			}
		})
	}
	failure := errors.New("database unavailable")
	if _, err := nodeActions(context.Background(), actionStore{err: failure}, uuid.New(), "active"); !errors.Is(err, failure) {
		t.Fatalf("lookup failed open: %v", err)
	}
}
