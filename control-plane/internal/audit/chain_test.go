package audit

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestChainPayloadIsUnambiguous(t *testing.T) {
	base := ChainRecord{EventID: uuid.Must(uuid.NewV7()), WorkspaceID: uuid.Must(uuid.NewV7()), At: time.Now().UTC(), ActorType: "user", ActorID: "operator", Action: "node.approve", ResourceType: "node", ResourceID: uuid.Must(uuid.NewV7())}
	first := base
	first.RequestID, first.Reason = "a|b", "c"
	second := base
	second.RequestID, second.Reason = "a", "b|c"
	firstPayload, err := encodeChainPayload([]byte("previous"), first)
	if err != nil {
		t.Fatal(err)
	}
	secondPayload, err := encodeChainPayload([]byte("previous"), second)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstPayload, secondPayload) {
		t.Fatal("distinct audit fields produced the same payload")
	}
	otherID := first
	otherID.EventID = uuid.Must(uuid.NewV7())
	otherPayload, err := encodeChainPayload([]byte("previous"), otherID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstPayload, otherPayload) {
		t.Fatal("distinct audit event IDs produced the same payload")
	}
	otherTrace := first
	otherTrace.TraceID = "0123456789abcdef0123456789abcdef"
	otherPayload, err = encodeChainPayload([]byte("previous"), otherTrace)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstPayload, otherPayload) {
		t.Fatal("distinct audit trace IDs produced the same payload")
	}
}
