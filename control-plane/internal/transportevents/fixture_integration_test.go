package transportevents

import (
	"context"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type commandResultFixture struct {
	pool         *pgxpool.Pool
	service      *Service
	signer       *commandauth.Signer
	workspaceID  uuid.UUID
	nodeID       uuid.UUID
	endpointID   [32]byte
	operationID  uuid.UUID
	commandID    uuid.UUID
	envelope     *agentv1.CommandEnvelope
	payloadHash  [32]byte
	traceparent  string
	issuedAt     time.Time
	originalTime time.Time
}

func newCommandResultFixture(t *testing.T) commandResultFixture {
	t.Helper()
	ctx := context.Background()
	pool := testfixture.OpenRuntimePool(t)
	operationID, commandID, key := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	messageID := uuid.Must(uuid.NewV7())
	now := time.Now().UTC().Truncate(time.Microsecond)
	traceparent := "00-1123456789abcdef0123456789abcdef-1123456789abcdef-01"
	signer := integrationCommandSigner()
	workspaceID := testfixture.InsertWorkspace(t, ctx, pool, "strict result", "strict-result")
	nodeID, endpointID := testfixture.InsertActiveNode(t, ctx, pool, workspaceID, "ocservia/development-simulator/")
	envelope := agentv1.CommandEnvelope{
		ProtocolVersion: commandauth.ProtocolVersion, MessageId: messageID[:], CommandId: commandID[:],
		IdempotencyKey: key[:], NodeId: nodeID[:], Sequence: 1,
		IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(time.Hour)),
		ExpectedRevision: 1, Traceparent: traceparent, ActorId: "test", Reason: "strict result validation",
		OperationId: operationID[:], Action: "operation.create", RequiredCapability: "synthetic.echo",
		DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY,
		Payload:      &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: "hello"}},
	}
	testutil.NoError(t, semanticpayload.PopulateV1(&envelope))
	testutil.NoError(t, signer.Authorize(&envelope))
	envelopeBytes, err := proto.Marshal(&envelope)
	testutil.NoError(t, err)
	testutil.Exec(t, ctx, pool, `INSERT INTO operations(id,workspace_id,node_id,command_id,state,request_id,trace_id,created_at,updated_at,idempotency_key,request_hash,expires_at) VALUES($1,$2,$3,$4,'dispatched','strict-result','1123456789abcdef0123456789abcdef',$5,$5,'strict-result',$6,$7)`, operationID, workspaceID, nodeID, commandID, now, make([]byte, 32), now.Add(time.Hour))
	testutil.Exec(t, ctx, pool, `INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,sequence,traceparent,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,'dispatched','synthetic_echo',$5,'strict-result',1,1,$6,$7,$8,$8)`, commandID, operationID, workspaceID, nodeID, envelopeBytes, traceparent, now.Add(time.Hour), now)
	payloadHash, err := semanticpayload.HashV1(&envelope)
	testutil.NoError(t, err)
	t.Cleanup(func() {
		for _, statement := range []string{
			`DELETE FROM operation_events WHERE operation_id=$1`,
			`DELETE FROM agent_command_results WHERE command_id=$2`,
			`DELETE FROM transport_events WHERE node_id=$3`,
			`DELETE FROM outbox_events WHERE command_id=$2`,
			`DELETE FROM commands WHERE id=$2`, `DELETE FROM operations WHERE id=$1`,
			`DELETE FROM node_endpoint_keys WHERE node_id=$3`,
			`DELETE FROM nodes WHERE id=$3`, `DELETE FROM workspaces WHERE id=$4`,
		} {
			_, _ = pool.Exec(context.Background(), statement, operationID, commandID, nodeID, workspaceID)
		}
	})
	return commandResultFixture{pool: pool, service: NewBackend(postgres.WrapPool(pool), signer), signer: signer, workspaceID: workspaceID, nodeID: nodeID, endpointID: endpointID, operationID: operationID, commandID: commandID, envelope: &envelope, payloadHash: payloadHash, traceparent: traceparent, issuedAt: now, originalTime: now}
}

func integrationCommandSigner() *commandauth.Signer {
	return testfixture.CommandSigner(6)
}

func integrationEndpoint(nodeID uuid.UUID) [32]byte {
	return testfixture.EndpointForNode("ocservia/development-simulator/", nodeID)
}

func (fixture *commandResultFixture) validResult() *agentv1.CommandResult {
	return &agentv1.CommandResult{
		CommandId: fixture.commandID[:], IdempotencyKey: fixture.envelope.GetIdempotencyKey(),
		PayloadSha256: fixture.payloadHash[:], State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED,
		Result: []byte("hello"), AcceptedAt: timestamppb.New(fixture.issuedAt), CompletedAt: timestamppb.New(fixture.issuedAt),
		SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1,
	}
}
