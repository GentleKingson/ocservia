package operations

import (
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/google/uuid"
)

func BenchmarkValidateSentEnvelope(b *testing.B) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	signer := testfixture.CommandSigner(1)

	build := func(payloadSize int) (Dispatch, []byte) {
		commandID := uuid.Must(uuid.NewV7())
		operationID := uuid.Must(uuid.NewV7())
		nodeID := uuid.Must(uuid.NewV7())

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
		return Dispatch{CommandID: commandID, OperationID: operationID, NodeID: nodeID, Envelope: claimed}, claimed
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
