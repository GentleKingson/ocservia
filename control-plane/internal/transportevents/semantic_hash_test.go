package transportevents

import (
	"testing"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
)

func v2Envelope(nodeID []byte, expectedRevision uint64, payloadKind uint32, message string) *agentv1.CommandEnvelope {
	envelope := &agentv1.CommandEnvelope{NodeId: nodeID, ExpectedRevision: expectedRevision}
	switch payloadKind {
	case 107:
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticNoop{SyntheticNoop: &agentv1.SyntheticNoop{}}
	case 108:
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: message}}
	default:
		panic("unsupported payload_kind in test")
	}
	return envelope
}

func TestCanonicalSemanticHashV2ExcludesDeliveryMetadata(t *testing.T) {
	node := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	a := v2Envelope(node, 1, 108, "hello")
	a.MessageId, a.CommandId, a.IdempotencyKey = []byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}, []byte{2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2}, []byte{3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3}
	a.Sequence, a.Traceparent, a.ActorId, a.Reason = 42, "00-aaa", "alice", "ticket-1"
	a.DeliveryMode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY
	b := v2Envelope(node, 1, 108, "hello")
	b.MessageId, b.CommandId, b.IdempotencyKey = []byte{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9}, []byte{8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8}, []byte{7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7}
	b.Sequence, b.Traceparent, b.ActorId, b.Reason = 99, "00-bbb", "bob", "ticket-2"
	b.DeliveryMode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY
	hashA, errA := semanticpayload.HashV2(a)
	hashB, errB := semanticpayload.HashV2(b)
	if errA != nil || errB != nil || hashA != hashB {
		t.Fatalf("identical semantic inputs must hash equally: %v, %v", errA, errB)
	}
}

func TestCanonicalSemanticHashV2ChangesOnSemanticInput(t *testing.T) {
	node := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	nodeShifted := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	hello, err := semanticpayload.HashV2(v2Envelope(node, 1, 108, "hello"))
	testutil.NoError(t, err)
	for _, tc := range []struct {
		name string
		env  *agentv1.CommandEnvelope
	}{
		{"message", v2Envelope(node, 1, 108, "world")},
		{"node", v2Envelope(nodeShifted, 1, 108, "hello")},
		{"revision", v2Envelope(node, 2, 108, "hello")},
		{"kind", v2Envelope(node, 1, 107, "")},
	} {
		got, err := semanticpayload.HashV2(tc.env)
		if err != nil || got == hello {
			t.Fatalf("%s change must change hash (err=%v)", tc.name, err)
		}
	}
}

func TestCanonicalSemanticHashV2RejectsUnicodeNormalization(t *testing.T) {
	node := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	composed, err := semanticpayload.HashV2(v2Envelope(node, 1, 108, "\u00e9"))
	testutil.NoError(t, err)
	decomposed, err := semanticpayload.HashV2(v2Envelope(node, 1, 108, "e\u0301"))
	testutil.NoError(t, err)
	if composed == decomposed {
		t.Fatal("no Unicode normalization: different bytes must hash differently")
	}
}
