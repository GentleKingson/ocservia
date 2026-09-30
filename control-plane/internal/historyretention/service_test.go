package historyretention

import (
	"bytes"
	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"
)

func TestCompactPreservesAuthorityAndRefusesLiveReplay(t *testing.T) {
	now := time.Now()
	id, node := uuid.New(), uuid.New()
	envelope := &agentv1.CommandEnvelope{CommandId: id[:], NodeId: node[:], IdempotencyKey: id[:], ExpectedRevision: 17, ExpiresAt: timestamppb.New(now.Add(-time.Hour)), SemanticPayloadSha256: bytes.Repeat([]byte{1}, 32), Reason: "retained signed reason", Payload: &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{}}}
	encoded, err := proto.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := compactEnvelope(Command{ID: id, NodeID: node, Envelope: encoded}, now)
	if err != nil {
		t.Fatal(err)
	}
	var restored agentv1.CommandEnvelope
	if err := proto.Unmarshal(compact, &restored); err != nil {
		t.Fatal(err)
	}
	envelope.Payload = nil
	if !proto.Equal(envelope, &restored) {
		t.Fatal("authority header changed")
	}
	if _, err := compactEnvelope(Command{ID: id, NodeID: node, Envelope: encoded}, now.Add(-2*time.Hour)); err == nil {
		t.Fatal("live signed envelope compacted")
	}
	if _, err := compactEnvelope(Command{ID: uuid.New(), NodeID: node, Envelope: encoded}, now); err == nil {
		t.Fatal("wrong command identity compacted")
	}
}
