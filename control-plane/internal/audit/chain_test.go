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

func TestLegacyMigrationFixturePayload(t *testing.T) {
	workspaceID := uuid.MustParse("00000000-0000-7000-8000-000000000021")
	eventID := uuid.MustParse("00000000-0000-7000-8000-000000000022")
	payload, err := encodeChainPayload(nil, ChainRecord{
		EventID: eventID, WorkspaceID: workspaceID, At: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
		ActorType: "controller", ActorID: "legacy", Action: "legacy.event", ResourceType: "workspace",
		ResourceID: workspaceID, RequestID: "legacy-preflight", Result: "succeeded",
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"previous":null,"event_id":"00000000-0000-7000-8000-000000000022","workspace_id":"00000000-0000-7000-8000-000000000021","occurred_at":"2026-08-12T00:00:00Z","actor_type":"controller","actor_id":"legacy","action":"legacy.event","resource_type":"workspace","resource_id":"00000000-0000-7000-8000-000000000021","request_id":"legacy-preflight","trace_id":"","result":"succeeded","reason":""}`
	if !bytes.Equal(payload, []byte(expected)) {
		t.Fatalf("legacy fixture payload = %s", payload)
	}
}
