package operations

import (
	"bytes"
	"crypto/ed25519"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func TestPrepareRecoveryEnvelopePreservesLogicalCommand(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	dispatch, sent := recoveryTestDispatch(t, now, time.Minute)
	var before agentv1.CommandEnvelope
	if err := proto.Unmarshal(sent, &before); err != nil {
		t.Fatal(err)
	}
	messageID := bytes.Clone(before.GetMessageId())
	semanticHash := bytes.Clone(before.GetSemanticPayloadSha256())

	signer := testCommandSigner(t)
	encoded, expiresAt, err := PrepareRecoveryEnvelope(
		&before,
		agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY,
		now.Add(2*time.Minute),
		signer,
	)
	if err != nil {
		t.Fatalf("prepare recovery envelope: %v", err)
	}
	var recovered agentv1.CommandEnvelope
	if err := proto.Unmarshal(encoded, &recovered); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recovered.GetCommandId(), dispatch.CommandID[:]) ||
		!bytes.Equal(recovered.GetOperationId(), dispatch.OperationID[:]) ||
		!bytes.Equal(recovered.GetNodeId(), dispatch.NodeID[:]) ||
		!bytes.Equal(recovered.GetIdempotencyKey(), before.GetIdempotencyKey()) {
		t.Fatal("recovery changed the logical command identity")
	}
	if bytes.Equal(recovered.GetMessageId(), messageID) {
		t.Fatal("recovery reused the dispatch-attempt message identity")
	}
	if recovered.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY ||
		recovered.GetConnectionFence() != nil || recovered.GetFenceBinding() != nil {
		t.Fatal("recovery did not clear old owner proofs and select reconcile-only delivery")
	}
	if recovered.GetSyntheticEcho().GetMessage() != "journaled" ||
		!bytes.Equal(recovered.GetSemanticPayloadSha256(), semanticHash) {
		t.Fatal("recovery changed the semantic command payload")
	}
	wantExpiry := now.Add(2*time.Minute + recoveryCommandTTL)
	if !expiresAt.Equal(wantExpiry) || !recovered.GetExpiresAt().AsTime().Equal(wantExpiry) {
		t.Fatalf("recovery expiry = %v/%v, want %v", expiresAt, recovered.GetExpiresAt().AsTime(), wantExpiry)
	}
	claims, err := commandauth.ClaimsFromEnvelopeV1(&recovered)
	if err != nil {
		t.Fatalf("project recovered authorization: %v", err)
	}
	canonical, err := commandauth.CanonicalV1(claims)
	if err != nil {
		t.Fatalf("canonicalize recovered authorization: %v", err)
	}
	if !ed25519.Verify(signer.PublicKey(), canonical, recovered.GetAuthorization().GetSignature()) {
		t.Fatal("recovery envelope was not re-authorized after changing attempt metadata")
	}
}

func TestValidateSentEnvelopeAllowsOnlyOwnerProofs(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	dispatch, sent := recoveryTestDispatch(t, now, 10*time.Minute)
	if err := validateSentEnvelope(dispatch, sent); err != nil {
		t.Fatalf("valid fenced send rejected: %v", err)
	}
	if err := validateSentEnvelope(dispatch, dispatch.Envelope); err != nil {
		t.Fatalf("valid unfenced send rejected: %v", err)
	}

	var changed agentv1.CommandEnvelope
	if err := proto.Unmarshal(sent, &changed); err != nil {
		t.Fatal(err)
	}
	changed.GetSyntheticEcho().Message = "changed"
	changedBytes, err := proto.Marshal(&changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSentEnvelope(dispatch, changedBytes); err == nil {
		t.Fatal("sent envelope changed the semantic payload")
	}

	if err := proto.Unmarshal(sent, &changed); err != nil {
		t.Fatal(err)
	}
	changed.FenceBinding.OwnerEpoch++
	changedBytes, err = proto.Marshal(&changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSentEnvelope(dispatch, changedBytes); err == nil {
		t.Fatal("sent envelope carried mismatched owner proofs")
	}
}

func recoveryTestDispatch(t *testing.T, now time.Time, ttl time.Duration) (Dispatch, []byte) {
	t.Helper()
	nodeID, operationID, commandID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	request := CreateRequest{
		NodeID: nodeID, IdempotencyKey: "recovery-test", ExpectedVersion: 1,
		Kind: SyntheticEcho, Message: "journaled", TTL: ttl, RequestID: "recovery-test",
		Traceparent: testTraceparent,
	}
	base, _, err := marshalEnvelope(request, operationID, commandID, 1, now, now.Add(ttl), testCommandSigner(t))
	if err != nil {
		t.Fatalf("marshal recovery fixture: %v", err)
	}
	dispatch := Dispatch{CommandID: commandID, OperationID: operationID, NodeID: nodeID, Envelope: base}
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(base, &envelope); err != nil {
		t.Fatal(err)
	}
	fenceID, connectionID, ownerID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	var node, connection, owner, command [16]byte
	copy(node[:], nodeID[:])
	copy(connection[:], connectionID[:])
	copy(owner[:], ownerID[:])
	copy(command[:], commandID[:])
	endpoint := [32]byte{1}
	signer := testCommandSigner(t)
	fence, err := signer.IssueConnectionFenceV2(
		[16]byte(fenceID), node, endpoint, owner, 7, 11, connection, 1,
		[]string{"ocserv.fencing.v2", "synthetic.echo"}, now.Add(time.Minute), now, now.Add(5*time.Minute),
	)
	if err != nil {
		t.Fatalf("issue recovery fixture fence: %v", err)
	}
	binding, err := signer.IssueFenceBindingV2(
		agentv1.FenceOperationKind_FENCE_OPERATION_KIND_COMMAND, command, [16]byte(fenceID), node, endpoint,
		owner, 7, 11, connection, 1, "synthetic.echo", now, now.Add(5*time.Minute),
	)
	if err != nil {
		t.Fatalf("issue recovery fixture binding: %v", err)
	}
	envelope.ConnectionFence, envelope.FenceBinding = fence, binding
	sent, err := proto.Marshal(&envelope)
	if err != nil {
		t.Fatal(err)
	}
	return dispatch, sent
}
