package userstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDesiredStateAtomicOfflineDriftVersionAndNodeScopeIntegration(t *testing.T) {
	service, pool, workspaceID, nodeID := integrationService(t, "offline")
	request := mutation(nodeID, "create-alice", UserCreate, "alice", 0)
	operation, replayed, err := service.Mutate(context.Background(), request)
	if err != nil || replayed || operation.State != "queued" {
		t.Fatalf("create=%+v replayed=%v err=%v", operation, replayed, err)
	}
	if _, replayed, err = service.Mutate(context.Background(), request); err != nil || !replayed {
		t.Fatalf("replay=%v %v", replayed, err)
	}

	var desiredVersion int64
	var envelope []byte
	var auditText string
	err = pool.QueryRow(context.Background(), `SELECT d.version,c.envelope,(SELECT string_agg(action||':'||reason,',') FROM audit_events WHERE workspace_id=$1) FROM desired_users d JOIN commands c ON c.node_id=d.node_id AND c.resource_key=d.username WHERE d.node_id=$2 AND d.username='alice' GROUP BY d.version,c.envelope`, workspaceID, nodeID).Scan(&desiredVersion, &envelope, &auditText)
	if err != nil || desiredVersion != 1 {
		t.Fatalf("atomic desired rows: version=%d err=%v", desiredVersion, err)
	}
	if strings.Contains(auditText, "plain-password-sentinel") {
		t.Fatal("password leaked to audit")
	}
	var command agentv1.CommandEnvelope
	if err := proto.Unmarshal(envelope, &command); err != nil {
		t.Fatal(err)
	}
	if command.GetUserCreate().GetUsername() != "alice" || len(command.GetUserCreate().GetSealedPassword()) != 64 {
		t.Fatalf("typed command=%v", &command)
	}

	otherNode := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(context.Background(), `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)VALUES($1,$2,$3,'offline',1,now(),now())`, otherNode, workspaceID, "other-"+otherNode.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,'ocserv.users.write',true)`, otherNode); err != nil {
		t.Fatal(err)
	}
	other := mutation(otherNode, "other-alice", UserCreate, "alice", 0)
	if _, _, err := service.Mutate(context.Background(), other); err != nil {
		t.Fatalf("same username on other node: %v", err)
	}

	stale := mutation(nodeID, "stale", UserDisable, "alice", 0)
	if _, _, err := service.Mutate(context.Background(), stale); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale error=%v", err)
	}
	observedFingerprint := desiredFingerprint(UserDisable, "alice", nil)
	if _, err := pool.Exec(context.Background(), `INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at)VALUES($1,'alice',false,1,$2,now())`, nodeID, observedFingerprint[:]); err != nil {
		t.Fatal(err)
	}
	states, err := service.List(context.Background(), nodeID)
	if err != nil || len(states) != 1 || states[0].Convergence != "offline_pending" {
		t.Fatalf("offline state=%+v err=%v", states, err)
	}

	if _, err := pool.Exec(context.Background(), `DELETE FROM observed_users WHERE node_id=$1 AND username='alice'`, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at)VALUES($1,'alice',true,1,$2,now())`, nodeID, bytes.Repeat([]byte{9}, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE nodes SET status='active' WHERE id=$1`, nodeID); err != nil {
		t.Fatal(err)
	}
	dispatches, err := operationstore.New(pool).Claim(context.Background(), uuid.Must(uuid.NewV7()), 8, time.Second)
	if err != nil || !slices.ContainsFunc(dispatches, func(dispatch operationstore.Dispatch) bool { return dispatch.NodeID == nodeID }) {
		t.Fatalf("online reconciliation did not release pending command: dispatches=%+v err=%v", dispatches, err)
	}
	states, err = service.List(context.Background(), nodeID)
	if err != nil || states[0].Convergence != "pending" {
		t.Fatalf("active drift with queued command=%+v err=%v", states, err)
	}
}

func TestEnableQueuesAuditableDesiredRevisionIntegration(t *testing.T) {
	service, pool, workspaceID, nodeID := integrationService(t, "active")
	disabled := desiredFingerprint(UserDisable, "alice", nil)
	if _, err := pool.Exec(context.Background(), `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES($1,'alice',false,3,3,$2,now(),now())`, nodeID, disabled[:]); err != nil {
		t.Fatal(err)
	}
	request := mutation(nodeID, "enable-alice", UserEnable, "alice", 3)
	operation, replayed, err := service.Mutate(context.Background(), request)
	if err != nil || replayed || operation.State != "queued" {
		t.Fatalf("enable=%+v replayed=%v err=%v", operation, replayed, err)
	}
	var enabled bool
	var version, revision int64
	var envelope []byte
	var auditAction string
	err = pool.QueryRow(context.Background(), `SELECT d.enabled,d.version,d.revision,c.envelope,(SELECT action FROM audit_events WHERE workspace_id=$1 AND command_id=c.id) FROM desired_users d JOIN commands c ON c.node_id=d.node_id AND c.resource_type='user' AND c.resource_key=d.username WHERE d.node_id=$2 AND d.username='alice'`, workspaceID, nodeID).Scan(&enabled, &version, &revision, &envelope, &auditAction)
	if err != nil || !enabled || version != 4 || revision != 4 || auditAction != "user.enable" {
		t.Fatalf("enabled=%v version=%d revision=%d audit=%q err=%v", enabled, version, revision, auditAction, err)
	}
	var command agentv1.CommandEnvelope
	if err := proto.Unmarshal(envelope, &command); err != nil {
		t.Fatal(err)
	}
	if command.GetUserEnable().GetUsername() != "alice" || command.GetUserEnable().GetDesiredRevision() != 4 {
		t.Fatalf("typed enable=%v", &command)
	}
}

func TestManagedResourceAndMembershipCapacityIntegration(t *testing.T) {
	service, pool, _, nodeID := integrationService(t, "active")
	if _, err := pool.Exec(context.Background(), `INSERT INTO desired_groups(node_id,group_name,members,version,revision,fingerprint,created_at,updated_at) SELECT $1,'group'||lpad(value::text,3,'0'),ARRAY[]::text[],1,1,decode(repeat('00',32),'hex'),now(),now() FROM generate_series(0,$2-1) value`, nodeID, MaxManagedResources); err != nil {
		t.Fatal(err)
	}
	request := mutation(nodeID, "capacity-group", GroupApply, "overflow", 0)
	if _, _, err := service.Mutate(context.Background(), request); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("managed group overflow was not rejected: %v", err)
	}
	first := mutation(nodeID, "membership-one", GroupApply, "group000", 1)
	first.Members = make([]string, MaxManagedResources)
	for index := range first.Members {
		first.Members[index] = fmt.Sprintf("user%03d", index)
	}
	if _, _, err := service.Mutate(context.Background(), first); err != nil {
		t.Fatalf("maximum aggregate membership: %v", err)
	}
	second := mutation(nodeID, "membership-overflow", GroupApply, "group001", 1)
	second.Members = []string{"alice"}
	if _, _, err := service.Mutate(context.Background(), second); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("aggregate membership overflow was not rejected: %v", err)
	}
}

func TestFreshObservedGroupMembershipsBoundApplyCapacityIntegration(t *testing.T) {
	service, pool, _, nodeID := integrationService(t, "active")
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }
	agentInstanceID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(context.Background(), `INSERT INTO node_observed_snapshots(node_id,observed_at,received_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,ocserv,system,path,last_heartbeat_at) VALUES($1,$2,$2,'group-capacity-boot',$3,'test-agent','test-ocserv','test-os','{}','{}','{}',$2)`, nodeID, now, agentInstanceID); err != nil {
		t.Fatal(err)
	}
	members := make([]string, MaxManagedResources)
	for index := range members {
		members[index] = fmt.Sprintf("user%03d", index)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO observed_groups(node_id,group_name,members,revision,fingerprint,observed_at) VALUES($1,'legacy',$2,0,decode(repeat('00',32),'hex'),$3)`, nodeID, members, now); err != nil {
		t.Fatal(err)
	}

	request := mutation(nodeID, "observed-membership-capacity", GroupApply, "new-group", 0)
	request.Members = []string{"alice"}
	if _, _, err := service.Mutate(context.Background(), request); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("fresh observed membership capacity was not rejected: %v", err)
	}
	var desired, operations, commands int
	if err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM desired_groups WHERE node_id=$1),(SELECT count(*) FROM operations WHERE node_id=$1),(SELECT count(*) FROM commands WHERE node_id=$1)`, nodeID).Scan(&desired, &operations, &commands); err != nil {
		t.Fatal(err)
	}
	if desired != 0 || operations != 0 || commands != 0 {
		t.Fatalf("capacity rejection persisted desired=%d operations=%d commands=%d", desired, operations, commands)
	}
}

func TestFreshObservedUsersBoundCreateCapacityIntegration(t *testing.T) {
	service, pool, _, nodeID := integrationService(t, "active")
	now := time.Now().UTC().Truncate(time.Microsecond)
	service.now = func() time.Time { return now }
	agentInstanceID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(context.Background(), `INSERT INTO node_observed_snapshots(node_id,observed_at,received_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,ocserv,system,path,last_heartbeat_at) VALUES($1,$2,$2,'capacity-boot',$3,'test-agent','test-ocserv','test-os','{}','{}','{}',$2)`, nodeID, now, agentInstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at) SELECT $1,'user'||lpad(value::text,3,'0'),true,0,decode(repeat('00',32),'hex'),$2 FROM generate_series(0,$3-1) value`, nodeID, now, MaxManagedResources); err != nil {
		t.Fatal(err)
	}

	request := mutation(nodeID, "observed-capacity-user", UserCreate, "overflow", 0)
	if _, _, err := service.Mutate(context.Background(), request); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("fresh observed user capacity was not rejected: %v", err)
	}
	var desired, operations, commands int
	if err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM desired_users WHERE node_id=$1),(SELECT count(*) FROM operations WHERE node_id=$1),(SELECT count(*) FROM commands WHERE node_id=$1)`, nodeID).Scan(&desired, &operations, &commands); err != nil {
		t.Fatal(err)
	}
	if desired != 0 || operations != 0 || commands != 0 {
		t.Fatalf("capacity rejection persisted desired=%d operations=%d commands=%d", desired, operations, commands)
	}
}

func TestI13IntentAndTerminalAuditIdentityMatchIntegration(t *testing.T) {
	tests := []struct {
		kind    MutationKind
		name    string
		version int64
		action  string
		members []string
	}{
		{UserCreate, "alice", 0, "user.create", nil},
		{UserPasswordRotate, "alice", 1, "user.password.rotate", nil},
		{UserDisable, "alice", 2, "user.disable", nil},
		{UserEnable, "alice", 3, "user.enable", nil},
		{GroupApply, "staff", 0, "group.apply", []string{"alice"}},
	}
	for _, test := range tests {
		for _, terminal := range []string{"succeeded", "failed"} {
			t.Run(test.action+"/"+terminal, func(t *testing.T) {
				service, pool, workspaceID, nodeID := integrationService(t, "active")
				ingest := localslice.New(pool)
				if test.kind != UserCreate && test.kind != GroupApply {
					enabled := test.kind != UserEnable
					fingerprint := desiredFingerprint(UserCreate, test.name, nil)
					if !enabled {
						fingerprint = desiredFingerprint(UserDisable, test.name, nil)
					}
					if _, err := pool.Exec(context.Background(), `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at) VALUES($1,$2,$3,$4,$4,$5,now(),now())`, nodeID, test.name, enabled, test.version, fingerprint[:]); err != nil {
						t.Fatal(err)
					}
				}
				request := mutation(nodeID, "terminal-audit-"+terminal+"-"+test.action, test.kind, test.name, test.version)
				request.Members = test.members
				operation, replayed, err := service.Mutate(context.Background(), request)
				if err != nil || replayed || operation.CommandID == nil {
					t.Fatalf("%s mutation=%+v replayed=%v err=%v", test.action, operation, replayed, err)
				}
				operationID := uuid.MustParse(operation.ID)
				commandID := uuid.MustParse(*operation.CommandID)
				var encoded []byte
				if err := pool.QueryRow(context.Background(), `UPDATE commands SET state='dispatched' WHERE id=$1 RETURNING envelope`, commandID).Scan(&encoded); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(context.Background(), `UPDATE operations SET state='dispatched' WHERE id=$1`, operationID); err != nil {
					t.Fatal(err)
				}
				var envelope agentv1.CommandEnvelope
				if err := proto.Unmarshal(encoded, &envelope); err != nil {
					t.Fatal(err)
				}
				completed := time.Now().UTC().Add(time.Millisecond)
				result := agentv1.CommandResult{
					CommandId: envelope.GetCommandId(), IdempotencyKey: envelope.GetIdempotencyKey(),
					PayloadSha256: envelope.GetSemanticPayloadSha256(), SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1,
					State:      agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED,
					Result:     []byte("applied"),
					AcceptedAt: timestamppb.New(completed), CompletedAt: timestamppb.New(completed),
				}
				if terminal == "failed" {
					result.State = agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED
					result.Result = nil
					result.ErrorCode = "privd_rejected"
				}
				payload, err := proto.Marshal(&result)
				if err != nil {
					t.Fatal(err)
				}
				eventID := uuid.Must(uuid.NewV7())
				if err := ingest.Ingest(context.Background(), &transportv1.TransportEvent{
					EventId: eventID[:], NodeId: nodeID[:],
					Type:       transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT,
					OccurredAt: timestamppb.New(completed), Traceparent: request.Traceparent, Payload: payload,
				}); err != nil {
					t.Fatalf("%s terminal result: %v", test.action, err)
				}

				rows, err := pool.Query(context.Background(), `SELECT action,resource_type,resource_id,node_id,command_id,request_id,COALESCE(trace_id,''),reason,result FROM audit_events WHERE command_id=$1 ORDER BY occurred_at,id`, commandID)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var results []string
				for rows.Next() {
					var action, resourceType, requestID, traceID, reason, resultState string
					var resourceID, auditedNodeID, auditedCommandID uuid.UUID
					if err := rows.Scan(&action, &resourceType, &resourceID, &auditedNodeID, &auditedCommandID, &requestID, &traceID, &reason, &resultState); err != nil {
						t.Fatal(err)
					}
					if action != test.action || resourceType != "operation" || resourceID != operationID || auditedNodeID != nodeID || auditedCommandID != commandID || requestID != request.RequestID || traceID != request.Traceparent[3:35] || reason != request.Reason {
						t.Fatalf("audit identity action=%q resource=%s/%s node=%s command=%s request=%q trace=%q reason=%q", action, resourceType, resourceID, auditedNodeID, auditedCommandID, requestID, traceID, reason)
					}
					results = append(results, resultState)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(results, []string{"intent", terminal}) {
					t.Fatalf("audit results=%v", results)
				}
				verification, err := audit.NewManager(pool, nil).Verify(context.Background(), workspaceID)
				if err != nil || !verification.Valid || verification.Events != 2 {
					t.Fatalf("audit chain=%+v err=%v", verification, err)
				}
				// The runtime test role intentionally cannot delete immutable results. Keep
				// retained fixtures compatible with the migration rollback exercised after
				// this package finishes.
				if _, err := pool.Exec(context.Background(), `UPDATE commands SET payload_type='synthetic_echo',expected_version=1 WHERE id=$1`, commandID); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestOfflineRevisionSlotBlocksChangesBehindCreateIntegration(t *testing.T) {
	service, pool, _, nodeID := integrationService(t, "offline")
	created, _, err := service.Mutate(context.Background(), mutation(nodeID, "alice-create", UserCreate, "alice", 0))
	if err != nil || created.CommandID == nil {
		t.Fatalf("create=%+v err=%v", created, err)
	}
	if _, _, err := service.Mutate(context.Background(), mutation(nodeID, "alice-disable", UserDisable, "alice", 1)); !errors.Is(err, ErrRevisionPending) {
		t.Fatalf("cross-kind mutation behind queued create=%v", err)
	}
	if _, _, err := service.Mutate(context.Background(), mutation(nodeID, "alice-create-replacement", UserCreate, "alice", 1)); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("queued create replacement=%v", err)
	}
	var originalState string
	var version, revision int64
	if err := pool.QueryRow(context.Background(), `SELECT state FROM commands WHERE id=$1`, uuid.MustParse(*created.CommandID)).Scan(&originalState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT version,revision FROM desired_users WHERE node_id=$1 AND username='alice'`, nodeID).Scan(&version, &revision); err != nil {
		t.Fatal(err)
	}
	if originalState != "queued" || version != 1 || revision != 1 {
		t.Fatalf("blocked state=%s desired=%d/%d", originalState, version, revision)
	}
}

func TestResourceMutationWaitsForPriorCrossKindRevisionIntegration(t *testing.T) {
	service, pool, _, nodeID := integrationService(t, "active")
	created, _, err := service.Mutate(context.Background(), mutation(nodeID, "ordered-create", UserCreate, "alice", 0))
	if err != nil || created.CommandID == nil {
		t.Fatalf("create setup=%+v err=%v", created, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE commands SET state='succeeded' WHERE id=$1`, uuid.MustParse(*created.CommandID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE operations SET state='succeeded',completed_at=now() WHERE id=$1`, uuid.MustParse(created.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE outbox_events SET published_at=now() WHERE command_id=$1`, uuid.MustParse(*created.CommandID)); err != nil {
		t.Fatal(err)
	}

	rotate, _, err := service.Mutate(context.Background(), mutation(nodeID, "ordered-rotate", UserPasswordRotate, "alice", 1))
	if err != nil || rotate.CommandID == nil {
		t.Fatalf("rotate=%+v err=%v", rotate, err)
	}
	if _, _, err := service.Mutate(context.Background(), mutation(nodeID, "ordered-disable", UserDisable, "alice", 2)); !errors.Is(err, ErrRevisionPending) {
		t.Fatalf("disable did not wait for rotate: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE commands SET state='succeeded' WHERE id=$1`, uuid.MustParse(*rotate.CommandID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE operations SET state='succeeded',completed_at=now() WHERE id=$1`, uuid.MustParse(rotate.ID)); err != nil {
		t.Fatal(err)
	}
	disable, _, err := service.Mutate(context.Background(), mutation(nodeID, "ordered-disable-after-success", UserDisable, "alice", 2))
	if err != nil || disable.CommandID == nil {
		t.Fatalf("disable after rotate=%+v err=%v", disable, err)
	}
}

func TestTerminalDesiredRevisionRequiresSameKindReplacementIntegration(t *testing.T) {
	markTerminal := func(t *testing.T, pool *pgxpool.Pool, operation operationstore.Operation, state string) {
		t.Helper()
		if operation.CommandID == nil {
			t.Fatal("operation has no command")
		}
		commandID := uuid.MustParse(*operation.CommandID)
		if _, err := pool.Exec(context.Background(), `UPDATE commands SET state=$2 WHERE id=$1`, commandID, state); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `UPDATE operations SET state=$2,completed_at=now() WHERE id=$1`, uuid.MustParse(operation.ID), state); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `UPDATE outbox_events SET published_at=now(),locked_by=NULL,locked_until=NULL WHERE command_id=$1`, commandID); err != nil {
			t.Fatal(err)
		}
	}
	assertEnvelope := func(t *testing.T, pool *pgxpool.Pool, operation operationstore.Operation, expected, desired uint64) {
		t.Helper()
		var encoded []byte
		if err := pool.QueryRow(context.Background(), `SELECT envelope FROM commands WHERE id=$1`, uuid.MustParse(*operation.CommandID)).Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(encoded, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.GetExpectedRevision() != expected {
			t.Fatalf("expected revision=%d want=%d", envelope.GetExpectedRevision(), expected)
		}
		actualDesired := uint64(0)
		switch payload := envelope.GetPayload().(type) {
		case *agentv1.CommandEnvelope_UserCreate:
			actualDesired = payload.UserCreate.GetDesiredRevision()
		case *agentv1.CommandEnvelope_UserPasswordRotate:
			actualDesired = payload.UserPasswordRotate.GetDesiredRevision()
		case *agentv1.CommandEnvelope_UserDisable:
			actualDesired = payload.UserDisable.GetDesiredRevision()
		case *agentv1.CommandEnvelope_UserEnable:
			actualDesired = payload.UserEnable.GetDesiredRevision()
		case *agentv1.CommandEnvelope_GroupApply:
			actualDesired = payload.GroupApply.GetDesiredRevision()
		}
		if actualDesired != desired {
			t.Fatalf("desired revision=%d want=%d", actualDesired, desired)
		}
	}
	assertRecovery := func(t *testing.T, service *Service, nodeID uuid.UUID, kind MutationKind, version int64) {
		t.Helper()
		states, err := service.List(context.Background(), nodeID)
		if err != nil || len(states) != 1 {
			t.Fatalf("recovery state=%+v err=%v", states, err)
		}
		if !states[0].RecoveryRequired || states[0].RecoveryMutationKind == nil || *states[0].RecoveryMutationKind != kind || states[0].DesiredVersion == nil || *states[0].DesiredVersion != version {
			t.Fatalf("recovery metadata=%+v want kind=%s version=%d", states[0], kind, version)
		}
	}

	t.Run("failed create", func(t *testing.T) {
		service, pool, _, nodeID := integrationService(t, "active")
		first, _, err := service.Mutate(context.Background(), mutation(nodeID, "failed-create", UserCreate, "alice", 0))
		if err != nil {
			t.Fatal(err)
		}
		markTerminal(t, pool, first, "failed")
		assertRecovery(t, service, nodeID, UserCreate, 1)
		recovery, _, err := service.Mutate(context.Background(), mutation(nodeID, "create-recovery", UserCreate, "alice", 1))
		if err != nil {
			t.Fatalf("create recovery: %v", err)
		}
		assertEnvelope(t, pool, recovery, 0, 1)
	})

	t.Run("failed password", func(t *testing.T) {
		service, pool, _, nodeID := integrationService(t, "active")
		fingerprint := desiredFingerprint(UserCreate, "alice", nil)
		if _, err := pool.Exec(context.Background(), `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES($1,'alice',true,1,1,$2,now(),now())`, nodeID, fingerprint[:]); err != nil {
			t.Fatal(err)
		}
		rotate, _, err := service.Mutate(context.Background(), mutation(nodeID, "failed-rotate", UserPasswordRotate, "alice", 1))
		if err != nil {
			t.Fatal(err)
		}
		markTerminal(t, pool, rotate, "failed")
		assertRecovery(t, service, nodeID, UserPasswordRotate, 2)
		if _, _, err := service.Mutate(context.Background(), mutation(nodeID, "disable-after-failed-password", UserDisable, "alice", 2)); !errors.Is(err, ErrRevisionRecovery) {
			t.Fatalf("cross-kind recovery error=%v", err)
		}
		recovery, _, err := service.Mutate(context.Background(), mutation(nodeID, "rotate-recovery", UserPasswordRotate, "alice", 2))
		if err != nil {
			t.Fatalf("password recovery: %v", err)
		}
		assertEnvelope(t, pool, recovery, 1, 2)
	})

	t.Run("expired group", func(t *testing.T) {
		service, pool, _, nodeID := integrationService(t, "offline")
		first, _, err := service.Mutate(context.Background(), mutation(nodeID, "expired-group", GroupApply, "staff", 0))
		if err != nil {
			t.Fatal(err)
		}
		markTerminal(t, pool, first, "expired")
		assertRecovery(t, service, nodeID, GroupApply, 1)
		recovery, _, err := service.Mutate(context.Background(), mutation(nodeID, "group-recovery", GroupApply, "staff", 1))
		if err != nil {
			t.Fatalf("group recovery: %v", err)
		}
		assertEnvelope(t, pool, recovery, 0, 1)
	})

	for _, test := range []struct {
		name            string
		kind            MutationKind
		initialEnabled  bool
		expectedEnabled bool
	}{
		{"failed disable", UserDisable, true, false},
		{"failed enable", UserEnable, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, pool, _, nodeID := integrationService(t, "active")
			fingerprint := desiredFingerprint(UserCreate, "alice", nil)
			if !test.initialEnabled {
				fingerprint = desiredFingerprint(UserDisable, "alice", nil)
			}
			if _, err := pool.Exec(context.Background(), `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES($1,'alice',$2,1,1,$3,now(),now())`, nodeID, test.initialEnabled, fingerprint[:]); err != nil {
				t.Fatal(err)
			}
			first, _, err := service.Mutate(context.Background(), mutation(nodeID, test.name, test.kind, "alice", 1))
			if err != nil {
				t.Fatal(err)
			}
			markTerminal(t, pool, first, "failed")
			assertRecovery(t, service, nodeID, test.kind, 2)

			recovery, _, err := service.Mutate(context.Background(), mutation(nodeID, test.name+" recovery", test.kind, "alice", 2))
			if err != nil {
				t.Fatalf("same-kind recovery: %v", err)
			}
			assertEnvelope(t, pool, recovery, 1, 2)
			markTerminal(t, pool, recovery, "succeeded")
			appliedFingerprint := desiredFingerprint(test.kind, "alice", nil)
			if _, err := pool.Exec(context.Background(), `INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at)VALUES($1,'alice',$2,2,$3,now())`, nodeID, test.expectedEnabled, appliedFingerprint[:]); err != nil {
				t.Fatal(err)
			}
			states, err := service.List(context.Background(), nodeID)
			if err != nil || len(states) != 1 || states[0].RecoveryRequired || states[0].RecoveryMutationKind != nil || states[0].Convergence != "converged" {
				t.Fatalf("successful recovery state=%+v err=%v", states, err)
			}
		})
	}
}

func TestSameKindSupersedeCoalescesAgentRevisionIntegration(t *testing.T) {
	service, pool, _, nodeID := integrationService(t, "active")
	created, _, err := service.Mutate(context.Background(), mutation(nodeID, "coalesce-create", UserCreate, "alice", 0))
	if err != nil || created.CommandID == nil {
		t.Fatalf("create setup=%+v err=%v", created, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE commands SET state='succeeded' WHERE id=$1`, uuid.MustParse(*created.CommandID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE operations SET state='succeeded',completed_at=now() WHERE id=$1`, uuid.MustParse(created.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE outbox_events SET published_at=now() WHERE command_id=$1`, uuid.MustParse(*created.CommandID)); err != nil {
		t.Fatal(err)
	}

	firstRotate, _, err := service.Mutate(context.Background(), mutation(nodeID, "coalesce-rotate-one", UserPasswordRotate, "alice", 1))
	if err != nil {
		t.Fatal(err)
	}
	secondRotate, _, err := service.Mutate(context.Background(), mutation(nodeID, "coalesce-rotate-two", UserPasswordRotate, "alice", 2))
	if err != nil || secondRotate.CommandID == nil {
		t.Fatalf("replacement rotate=%+v err=%v", secondRotate, err)
	}
	assertCoalesced := func(first, second operationstore.Operation, payload func(*agentv1.CommandEnvelope) (uint64, uint64), expectedVersion, expectedRevision, desiredRevision int64) {
		t.Helper()
		if first.CommandID == nil || second.CommandID == nil {
			t.Fatal("coalesced operations must have command ids")
		}
		var firstState, secondState string
		var storedExpected int64
		var encoded []byte
		if err := pool.QueryRow(context.Background(), `SELECT state FROM commands WHERE id=$1`, uuid.MustParse(*first.CommandID)).Scan(&firstState); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(context.Background(), `SELECT state,expected_version,envelope FROM commands WHERE id=$1`, uuid.MustParse(*second.CommandID)).Scan(&secondState, &storedExpected, &encoded); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(encoded, &envelope); err != nil {
			t.Fatal(err)
		}
		actualExpected, actualDesired := payload(&envelope)
		if firstState != "superseded" || secondState != "queued" || storedExpected != expectedVersion || actualExpected != uint64(expectedRevision) || actualDesired != uint64(desiredRevision) {
			t.Fatalf("coalesced states=%s/%s stored_expected=%d envelope=%d->%d", firstState, secondState, storedExpected, actualExpected, actualDesired)
		}
	}
	assertCoalesced(firstRotate, secondRotate, func(envelope *agentv1.CommandEnvelope) (uint64, uint64) {
		return envelope.GetExpectedRevision(), envelope.GetUserPasswordRotate().GetDesiredRevision()
	}, 1, 1, 2)
	var userVersion, userRevision int64
	if err := pool.QueryRow(context.Background(), `SELECT version,revision FROM desired_users WHERE node_id=$1 AND username='alice'`, nodeID).Scan(&userVersion, &userRevision); err != nil {
		t.Fatal(err)
	}
	if userVersion != 3 || userRevision != 2 {
		t.Fatalf("coalesced user version/revision=%d/%d", userVersion, userRevision)
	}

	firstGroup, _, err := service.Mutate(context.Background(), mutation(nodeID, "coalesce-group-one", GroupApply, "staff", 0))
	if err != nil {
		t.Fatal(err)
	}
	secondGroup, _, err := service.Mutate(context.Background(), mutation(nodeID, "coalesce-group-two", GroupApply, "staff", 1))
	if err != nil {
		t.Fatal(err)
	}
	assertCoalesced(firstGroup, secondGroup, func(envelope *agentv1.CommandEnvelope) (uint64, uint64) {
		return envelope.GetExpectedRevision(), envelope.GetGroupApply().GetDesiredRevision()
	}, 0, 0, 1)
	var groupVersion, groupRevision int64
	if err := pool.QueryRow(context.Background(), `SELECT version,revision FROM desired_groups WHERE node_id=$1 AND group_name='staff'`, nodeID).Scan(&groupVersion, &groupRevision); err != nil {
		t.Fatal(err)
	}
	if groupVersion != 2 || groupRevision != 1 {
		t.Fatalf("coalesced group version/revision=%d/%d", groupVersion, groupRevision)
	}

	dispatcher := operationstore.New(pool)
	claimed := map[uuid.UUID]bool{}
	for range 2 {
		dispatches, err := dispatcher.Claim(context.Background(), uuid.Must(uuid.NewV7()), 8, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		for _, dispatch := range dispatches {
			if dispatch.NodeID != nodeID {
				continue
			}
			claimed[dispatch.CommandID] = true
			if err := dispatcher.MarkSent(context.Background(), dispatch); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !claimed[uuid.MustParse(*secondRotate.CommandID)] || !claimed[uuid.MustParse(*secondGroup.CommandID)] {
		t.Fatalf("coalesced commands did not become dispatchable: %+v", claimed)
	}
}

func TestRejectedRevisionSlotRequiresProofThatNoEffectWasAcceptedIntegration(t *testing.T) {
	ingestResult := func(t *testing.T, pool *pgxpool.Pool, nodeID uuid.UUID, operation operationstore.Operation, state agentv1.CommandResultState, errorCode string) {
		t.Helper()
		commandID := uuid.MustParse(*operation.CommandID)
		var encoded []byte
		if err := pool.QueryRow(context.Background(), `UPDATE commands SET state=CASE WHEN state='queued' THEN 'dispatched' ELSE state END WHERE id=$1 RETURNING envelope`, commandID).Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `UPDATE operations SET state=CASE WHEN state='queued' THEN 'dispatched' ELSE state END WHERE id=$1`, uuid.MustParse(operation.ID)); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(encoded, &envelope); err != nil {
			t.Fatal(err)
		}
		completed := time.Now().UTC().Add(time.Millisecond)
		result := agentv1.CommandResult{
			CommandId: envelope.GetCommandId(), IdempotencyKey: envelope.GetIdempotencyKey(),
			State: state, ErrorCode: errorCode, CompletedAt: timestamppb.New(completed),
		}
		if state == agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN {
			result.PayloadSha256 = envelope.GetSemanticPayloadSha256()
			result.SemanticPayloadHashVersion = agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1
			result.AcceptedAt = timestamppb.New(completed)
		}
		payload, err := proto.Marshal(&result)
		if err != nil {
			t.Fatal(err)
		}
		eventID := uuid.Must(uuid.NewV7())
		if err := localslice.New(pool).Ingest(context.Background(), &transportv1.TransportEvent{
			EventId: eventID[:], NodeId: nodeID[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT,
			OccurredAt: timestamppb.New(completed), Traceparent: testTraceparent, Payload: payload,
		}); err != nil {
			t.Fatalf("ingest %s result: %v", state, err)
		}
	}
	seed := func(t *testing.T, pool *pgxpool.Pool, nodeID uuid.UUID) {
		t.Helper()
		fingerprint := desiredFingerprint(UserCreate, "alice", nil)
		if _, err := pool.Exec(context.Background(), `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at)VALUES($1,'alice',true,1,1,$2,now(),now())`, nodeID, fingerprint[:]); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("pre-effect rejection permits same-kind slot reuse", func(t *testing.T) {
		service, pool, _, nodeID := integrationService(t, "active")
		seed(t, pool, nodeID)
		rotate, _, err := service.Mutate(context.Background(), mutation(nodeID, "rejected-rotate", UserPasswordRotate, "alice", 1))
		if err != nil {
			t.Fatal(err)
		}
		ingestResult(t, pool, nodeID, rotate, agentv1.CommandResultState_COMMAND_RESULT_STATE_REJECTED, "command_expired")
		states, err := service.List(context.Background(), nodeID)
		if err != nil || len(states) != 1 || !states[0].RecoveryRequired || states[0].RecoveryMutationKind == nil || *states[0].RecoveryMutationKind != UserPasswordRotate {
			t.Fatalf("safe rejected metadata=%+v err=%v", states, err)
		}
		replacement, _, err := service.Mutate(context.Background(), mutation(nodeID, "replacement-rotate", UserPasswordRotate, "alice", 2))
		if err != nil {
			t.Fatalf("pre-effect replacement: %v", err)
		}
		var encoded []byte
		if err := pool.QueryRow(context.Background(), `SELECT envelope FROM commands WHERE id=$1`, uuid.MustParse(*replacement.CommandID)).Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(encoded, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.GetExpectedRevision() != 1 || envelope.GetUserPasswordRotate().GetDesiredRevision() != 2 {
			t.Fatalf("replacement revisions expected=%d desired=%d", envelope.GetExpectedRevision(), envelope.GetUserPasswordRotate().GetDesiredRevision())
		}
		// The runtime test role cannot delete immutable Agent results. Keep the
		// retained rejected command compatible with migration rollback.
		if _, err := pool.Exec(context.Background(), `UPDATE commands SET payload_type='synthetic_echo',expected_version=1 WHERE workspace_id=(SELECT workspace_id FROM commands WHERE id=$1)`, uuid.MustParse(*rotate.CommandID)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("accepted unknown cannot be retired by a later rejection", func(t *testing.T) {
		service, pool, _, nodeID := integrationService(t, "active")
		seed(t, pool, nodeID)
		rotate, _, err := service.Mutate(context.Background(), mutation(nodeID, "unknown-rotate", UserPasswordRotate, "alice", 1))
		if err != nil {
			t.Fatal(err)
		}
		ingestResult(t, pool, nodeID, rotate, agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN, "privd_outcome_unknown")
		ingestResult(t, pool, nodeID, rotate, agentv1.CommandResultState_COMMAND_RESULT_STATE_REJECTED, "command_expired")
		states, err := service.List(context.Background(), nodeID)
		if err != nil || len(states) != 1 || !states[0].RecoveryRequired || states[0].RecoveryMutationKind != nil {
			t.Fatalf("ambiguous rejected metadata=%+v err=%v", states, err)
		}
		if _, _, err := service.Mutate(context.Background(), mutation(nodeID, "unsafe-replacement", UserPasswordRotate, "alice", 2)); !errors.Is(err, ErrRevisionRecovery) {
			t.Fatalf("ambiguous rejected replacement=%v", err)
		}
		if _, err := pool.Exec(context.Background(), `UPDATE commands SET payload_type='synthetic_echo',expected_version=1 WHERE workspace_id=(SELECT workspace_id FROM commands WHERE id=$1)`, uuid.MustParse(*rotate.CommandID)); err != nil {
			t.Fatal(err)
		}
	})
}

func mutation(nodeID uuid.UUID, key string, kind MutationKind, name string, version int64) MutationRequest {
	request := MutationRequest{NodeID: nodeID, Kind: kind, Name: name, IdempotencyKey: key, ExpectedVersion: version, TTL: time.Hour, ActorID: "operator", Reason: "ticket", RequestID: "request-" + key, Traceparent: testTraceparent}
	if kind == UserCreate || kind == UserPasswordRotate {
		request.SealedPassword = bytes.Repeat([]byte{0xa5}, 64)
		request.SecretKeyID = "node-key-1"
	}
	return request
}

func TestGlobalCommandLimitAcrossProducersIntegration(t *testing.T) {
	_, pool, _, nodeID := integrationService(t, "active")
	users := NewWithConcurrency(pool, 1)
	operations := operationstore.NewWithConcurrency(pool, 1)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, _, err := users.Mutate(context.Background(), mutation(nodeID, "limited-user", UserCreate, "limited", 0))
		results <- err
	}()
	go func() {
		<-start
		_, _, err := operations.CreateSynthetic(context.Background(), operationstore.CreateRequest{NodeID: nodeID, IdempotencyKey: "limited-noop", ExpectedVersion: 1, Kind: operationstore.SyntheticNoop, TTL: time.Minute, RequestID: "request-limited-noop", Traceparent: testTraceparent})
		results <- err
	}()
	close(start)

	var succeeded, limited int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrConcurrencyExceeded), errors.Is(err, operationstore.ErrConcurrencyExceeded):
			limited++
		default:
			t.Fatalf("unexpected command admission error: %v", err)
		}
	}
	var active int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM operations operation JOIN commands command ON command.operation_id=operation.id WHERE operation.state IN('queued','dispatched','accepted','running','offline_pending','unknown')`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || limited != 1 || active != 1 {
		t.Fatalf("global limit result succeeded=%d limited=%d active=%d", succeeded, limited, active)
	}
}

func integrationService(t *testing.T, status string) (*Service, *pgxpool.Pool, uuid.UUID, uuid.UUID) {
	t.Helper()
	url := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	_, err = pool.Exec(context.Background(), `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'I13 test',$2,now(),now())`, workspaceID, "i13-"+workspaceID.String())
	if err == nil {
		_, err = pool.Exec(context.Background(), `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)VALUES($1,$2,$3,$4,1,now(),now())`, nodeID, workspaceID, "node-"+nodeID.String(), status)
	}
	if err == nil {
		_, err = pool.Exec(context.Background(), `INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,'ocserv.users.write',true),($1,'ocserv.groups.write',true)`, nodeID)
	}
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, statement := range []string{`DELETE FROM agent_command_results WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM transport_events WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, `DELETE FROM node_command_leases WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM command_attempts WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id=$1)`, `DELETE FROM commands WHERE workspace_id=$1`, `DELETE FROM operations WHERE workspace_id=$1`, `DELETE FROM audit_events WHERE workspace_id=$1`, `DELETE FROM observed_groups WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, `DELETE FROM observed_users WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, `DELETE FROM desired_groups WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, `DELETE FROM desired_users WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, `DELETE FROM node_capabilities WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, `DELETE FROM nodes WHERE workspace_id=$1`, `DELETE FROM workspaces WHERE id=$1`} {
			_, _ = pool.Exec(context.Background(), statement, workspaceID)
		}
		pool.Close()
	})
	return New(pool), pool, workspaceID, nodeID
}
