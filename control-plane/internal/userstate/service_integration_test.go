package userstate

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
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

func TestOfflineSupersedePreservesNonSubstitutableUserCommandsIntegration(t *testing.T) {
	service, pool, _, nodeID := integrationService(t, "offline")
	apply := func(key string, kind MutationKind, name string, version int64) {
		t.Helper()
		if _, _, err := service.Mutate(context.Background(), mutation(nodeID, key, kind, name, version)); err != nil {
			t.Fatalf("%s %s: %v", name, kind, err)
		}
	}
	apply("alice-create", UserCreate, "alice", 0)
	apply("alice-rotate", UserPasswordRotate, "alice", 1)
	apply("alice-disable", UserDisable, "alice", 2)
	apply("bob-create", UserCreate, "bob", 0)
	apply("bob-disable", UserDisable, "bob", 1)
	apply("bob-rotate", UserPasswordRotate, "bob", 2)
	apply("carol-create", UserCreate, "carol", 0)
	apply("carol-disable", UserDisable, "carol", 1)
	apply("carol-enable", UserEnable, "carol", 2)
	apply("dave-create", UserCreate, "dave", 0)
	apply("dave-rotate-1", UserPasswordRotate, "dave", 1)
	apply("dave-rotate-2", UserPasswordRotate, "dave", 2)
	groupOne := mutation(nodeID, "staff-1", GroupApply, "staff", 0)
	groupOne.Members = []string{"alice"}
	if _, _, err := service.Mutate(context.Background(), groupOne); err != nil {
		t.Fatal(err)
	}
	groupTwo := mutation(nodeID, "staff-2", GroupApply, "staff", 1)
	groupTwo.Members = []string{"alice", "bob"}
	if _, _, err := service.Mutate(context.Background(), groupTwo); err != nil {
		t.Fatal(err)
	}

	assertStates := func(name string, want map[MutationKind]map[string]int) {
		t.Helper()
		rows, err := pool.Query(context.Background(), `SELECT payload_type,state,count(*) FROM commands WHERE node_id=$1 AND resource_type='user' AND resource_key=$2 GROUP BY payload_type,state`, nodeID, name)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[MutationKind]map[string]int{}
		for rows.Next() {
			var kind MutationKind
			var state string
			var count int
			if err := rows.Scan(&kind, &state, &count); err != nil {
				t.Fatal(err)
			}
			if got[kind] == nil {
				got[kind] = map[string]int{}
			}
			got[kind][state] = count
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if !mapsEqual(got, want) {
			t.Fatalf("%s command states=%v want=%v", name, got, want)
		}
	}
	queued := map[string]int{"queued": 1}
	assertStates("alice", map[MutationKind]map[string]int{UserCreate: queued, UserPasswordRotate: queued, UserDisable: queued})
	assertStates("bob", map[MutationKind]map[string]int{UserCreate: queued, UserDisable: queued, UserPasswordRotate: queued})
	assertStates("carol", map[MutationKind]map[string]int{UserCreate: queued, UserDisable: {"superseded": 1}, UserEnable: queued})
	assertStates("dave", map[MutationKind]map[string]int{UserCreate: queued, UserPasswordRotate: {"queued": 1, "superseded": 1}})
	var queuedGroups, supersededGroups int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FILTER(WHERE state='queued'),count(*) FILTER(WHERE state='superseded') FROM commands WHERE node_id=$1 AND resource_type='group' AND resource_key='staff'`, nodeID).Scan(&queuedGroups, &supersededGroups); err != nil {
		t.Fatal(err)
	}
	if queuedGroups != 1 || supersededGroups != 1 {
		t.Fatalf("group supersede queued=%d superseded=%d", queuedGroups, supersededGroups)
	}
}

func mapsEqual(left, right map[MutationKind]map[string]int) bool {
	return maps.EqualFunc(left, right, func(leftStates, rightStates map[string]int) bool {
		return maps.Equal(leftStates, rightStates)
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
		for _, statement := range []string{`DELETE FROM agent_command_results WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM node_command_leases WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM command_attempts WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM commands WHERE workspace_id=$1`, `DELETE FROM operations WHERE workspace_id=$1`, `DELETE FROM audit_events WHERE workspace_id=$1`, `DELETE FROM observed_groups WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, `DELETE FROM observed_users WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, `DELETE FROM desired_groups WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, `DELETE FROM desired_users WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, `DELETE FROM node_capabilities WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, `DELETE FROM nodes WHERE workspace_id=$1`, `DELETE FROM workspaces WHERE id=$1`} {
			_, _ = pool.Exec(context.Background(), statement, workspaceID)
		}
		pool.Close()
	})
	return New(pool), pool, workspaceID, nodeID
}
