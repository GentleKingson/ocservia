package localslice

import (
	"context"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	resultstore "github.com/GentleKingson/ocservia/control-plane/internal/localslice/store"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type recoveryResultStore struct {
	resultstore.ResultStore
	payload []byte
}

func (s *recoveryResultStore) ScheduleRecovery(_ context.Context, _ uuid.UUID, payload []byte, _, _ value.Timestamp) error {
	s.payload = payload
	return nil
}

func TestEffectAbsenceCannotRenewExecutionAuthority(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, protocol := range []string{commandauth.ProtocolVersion, commandauth.LegacyProtocolVersion} {
		for _, elapsed := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
			id := uuid.Must(uuid.NewV7())
			command := &agentv1.CommandEnvelope{ProtocolVersion: protocol, CommandId: id[:], IdempotencyKey: id[:], NodeId: id[:], OperationId: id[:], IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(time.Minute)), ExpectedRevision: 1, ActorId: "test", Action: "operation.create", RequiredCapability: "synthetic.echo", SemanticPayloadHashVersion: 2, SemanticPayloadSha256: make([]byte, 32), DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY, Payload: &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: "recovery"}}}
			store := &recoveryResultStore{}
			if err := scheduleCommandRecovery(t.Context(), store, id, command, "effect_absent", now.Add(elapsed), commandauth.NewSignerFromSeed([32]byte{1})); err != nil {
				t.Fatal(err)
			}
			allowed := protocol == commandauth.ProtocolVersion && elapsed < time.Minute
			if (len(store.payload) != 0) != allowed {
				t.Fatalf("protocol=%s elapsed=%v scheduled=%v", protocol, elapsed, len(store.payload) != 0)
			}
			if allowed {
				var retry agentv1.CommandEnvelope
				if err := proto.Unmarshal(store.payload, &retry); err != nil {
					t.Fatal(err)
				}
				if retry.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RETRY_IF_EFFECT_ABSENT || !retry.GetExpiresAt().AsTime().Equal(now.Add(time.Minute)) {
					t.Fatal("retry changed original execution deadline")
				}
			}
		}
	}
}
