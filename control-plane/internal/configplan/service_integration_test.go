package configplan

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestConfigPlanCreateReplayStaleAndTypedEnvelopeIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ownerPool := pool
	if ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL"); ownerURL != "" {
		ownerPool, err = pgxpool.New(ctx, ownerURL)
		if err != nil {
			t.Fatal(err)
		}
		defer ownerPool.Close()
	}
	workspaceID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err = pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'I15 test',$2,now(),now())`, workspaceID, "i15-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)VALUES($1,$2,$3,'active',1,now(),now())`, nodeID, workspaceID, "node-"+nodeID.String()); err != nil {
		t.Fatal(err)
	}
	for _, capability := range []string{"ocserv.config.plan", "config.network", "config.limits", "config.runtime", "config.auth"} {
		if _, err = pool.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,$2,true)`, nodeID, capability); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := cleanupConfigPlanIntegration(context.Background(), ownerPool, workspaceID); err != nil {
			t.Error(err)
		}
	})

	service := New(pool, operations.New(pool))
	request := CreateRequest{NodeID: nodeID, ExpectedRevision: 0, Template: Template{Name: "baseline", Directives: []Directive{
		{Name: "auth", Value: "plain[passwd=/etc/ocserv/ocpasswd]"}, {Name: "max-clients", Value: "128"},
		{Name: "socket-file", Value: "/run/ocserv.socket"}, {Name: "tcp-port", Value: "443"},
	}}, TTL: 15 * time.Minute, IdempotencyKey: "i15-plan", ActorID: "developer", RequestID: "request-i15", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Reason: "integration plan"}
	plan, replayed, err := service.Create(ctx, request)
	if err != nil || replayed || plan.ExpectedRevision != 0 || plan.Validation != "pending" {
		t.Fatalf("create plan=%+v replayed=%v err=%v", plan, replayed, err)
	}
	second, replayed, err := service.Create(ctx, request)
	if err != nil || !replayed || second.ID != plan.ID {
		t.Fatalf("replay plan=%+v replayed=%v err=%v", second, replayed, err)
	}
	var envelopeBytes []byte
	if err := pool.QueryRow(ctx, `SELECT c.envelope FROM commands c WHERE c.operation_id=$1`, plan.OperationID).Scan(&envelopeBytes); err != nil {
		t.Fatal(err)
	}
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(envelopeBytes, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.GetConfigPlan() == nil || len(envelope.GetConfigPlan().GetCandidateHash()) != 32 || envelope.GetExpectedRevision() != 0 {
		t.Fatalf("typed envelope=%v", &envelope)
	}
	commandID := uuid.Must(uuid.FromBytes(envelope.GetCommandId()))
	eventID := uuid.Must(uuid.NewV7())
	hash, _ := hex.DecodeString(plan.CandidateHash)
	var candidateRedacted string
	if err := pool.QueryRow(ctx, `SELECT candidate_redacted FROM config_plans WHERE id=$1`, plan.ID).Scan(&candidateRedacted); err != nil {
		t.Fatal(err)
	}
	resultBytes, err := proto.Marshal(&agentv1.ConfigPlanResult{CandidateHash: hash, DiffRedacted: safeDiff(candidateRedacted), CurrentUnchanged: true, StagingCleaned: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload)VALUES($1,$2,'command_result',now(),$3,''::bytea)`, eventID, nodeID, request.Traceparent); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO agent_command_results(event_id,command_id,idempotency_key,payload_sha256,semantic_payload_hash_version,state,result,accepted_at,completed_at,replayed,created_at)VALUES($1,$2,$3,$4,1,'succeeded',$5,now(),now(),false,now())`, eventID, commandID, uuid.Must(uuid.FromBytes(envelope.GetIdempotencyKey())), envelope.GetSemanticPayloadSha256(), resultBytes); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE commands SET state='succeeded',updated_at=now() WHERE id=$1`, commandID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE operations SET state='succeeded',updated_at=now() WHERE id=$1`, plan.OperationID); err != nil {
		t.Fatal(err)
	}
	validated, err := service.Get(ctx, plan.ID)
	if err != nil || validated.Validation != "valid" || !validated.CurrentUnchanged || !validated.StagingCleaned {
		t.Fatalf("validated plan=%+v err=%v", validated, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_config_state(node_id,revision,redacted_config,updated_at)VALUES($1,1,'',now())`, nodeID); err != nil {
		t.Fatal(err)
	}
	request.IdempotencyKey = "stale-plan"
	if _, _, err := service.Create(ctx, request); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale revision error=%v", err)
	}
}

func cleanupConfigPlanIntegration(ctx context.Context, pool *pgxpool.Pool, workspaceID uuid.UUID) error {
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only`); err != nil {
		return err
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only`)
	}()
	statements := []string{
		`DELETE FROM config_plans WHERE workspace_id=$1`,
		`DELETE FROM node_config_state WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM agent_command_results WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM transport_events WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM node_command_leases WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM command_attempts WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
		`DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id=$1)`,
		`DELETE FROM audit_events WHERE workspace_id=$1`,
		`DELETE FROM commands WHERE workspace_id=$1`,
		`DELETE FROM operations WHERE workspace_id=$1`,
		`DELETE FROM node_capabilities WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
		`DELETE FROM nodes WHERE workspace_id=$1`,
		`DELETE FROM workspaces WHERE id=$1`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement, workspaceID); err != nil {
			return err
		}
	}
	return nil
}
