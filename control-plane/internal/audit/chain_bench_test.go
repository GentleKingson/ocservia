package audit

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"
)

func BenchmarkEncodeChainPayload(b *testing.B) {
	prev := make([]byte, sha256.Size)
	record := ChainRecord{
		EventID:      uuid.Must(uuid.NewV7()),
		WorkspaceID:  uuid.Must(uuid.NewV7()),
		At:           time.Now().UTC().Truncate(time.Microsecond),
		ActorType:    "admin",
		ActorID:      "operator-1",
		Action:       "operation.create",
		ResourceType: "node",
		ResourceID:   uuid.Must(uuid.NewV7()),
		RequestID:    "req-bench-0001",
		TraceID:      "0123456789abcdef0123456789abcdef",
		Result:       "succeeded",
		Reason:       "scheduled rollout",
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := encodeChainPayload(prev, record); err != nil {
			b.Fatal(err)
		}
	}
}
