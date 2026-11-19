package operations

import (
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func BenchmarkValidateSentEnvelope(b *testing.B) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	signer := testfixture.CommandSigner(1)

	build := func(payloadSize int) (Dispatch, []byte) {
		commandID := uuid.Must(uuid.NewV7())
		operationID := uuid.Must(uuid.NewV7())
		nodeID := uuid.Must(uuid.NewV7())
		fenceID := uuid.Must(uuid.NewV7())
		ownerID := uuid.Must(uuid.NewV7())
		connID := uuid.Must(uuid.NewV7())

		req := CreateRequest{
			NodeID:          nodeID,
			IdempotencyKey:  "bench-key",
			ExpectedVersion: 1,
			TTL:             5 * time.Minute,
			RequestID:       "bench-req",
			Traceparent:     testTraceparent,
			Payload:         SyntheticEchoPayload{Message: strings.Repeat("a", payloadSize)},
		}
		claimed, _, err := marshalEnvelope(req, operationID, commandID, 1, now, now.Add(5*time.Minute), signer)
		if err != nil {
			b.Fatal(err)
		}
		var env agentv1.CommandEnvelope
		if err := proto.Unmarshal(claimed, &env); err != nil {
			b.Fatal(err)
		}
		endpoint := [32]byte{1}
		fence, err := signer.IssueConnectionFenceV2(
			[16]byte(fenceID), [16]byte(nodeID), endpoint, [16]byte(ownerID), 7, 11, [16]byte(connID), 1,
			[]string{"ocserv.fencing.v2", "synthetic.echo"}, now.Add(time.Minute), now, now.Add(5*time.Minute),
		)
		if err != nil {
			b.Fatal(err)
		}
		binding, err := signer.IssueFenceBindingV2(
			agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, [16]byte(commandID), [16]byte(fenceID),
			[16]byte(nodeID), endpoint, [16]byte(ownerID), 7, 11, [16]byte(connID), 1, "synthetic.echo", now, now.Add(5*time.Minute),
		)
		if err != nil {
			b.Fatal(err)
		}
		env.ConnectionFence = fence
		env.FenceBinding = binding
		sent, err := proto.Marshal(&env)
		if err != nil {
			b.Fatal(err)
		}
		return Dispatch{CommandID: commandID, OperationID: operationID, NodeID: nodeID, Envelope: claimed}, sent
	}

	for _, tc := range []struct {
		name string
		size int
	}{{"Small_64B", 64}, {"MaxEcho_4KiB", 4096}} {
		dispatch, sent := build(tc.size)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(sent)))
			for b.Loop() {
				if _, err := validateAndDecodeSentEnvelope(dispatch, sent); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
