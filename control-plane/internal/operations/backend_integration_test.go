//go:build integration

package operations_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
)

type outboxFixture struct {
	backend, owner *postgres.Backend
	workspace      uuid.UUID
	signer         *commandauth.Signer
}

var (
	outboxIdentityID = uuid.MustParse("019aaf00-0000-7000-8000-000000000001")
	outboxSessionID  = uuid.MustParse("019aaf00-0000-7000-8000-000000000002")
	lostCommit       = errors.New("injected lost commit acknowledgement")
)

func newOutboxFixture(t *testing.T) *outboxFixture {
	t.Helper()
	f := &outboxFixture{workspace: uuid.Must(uuid.NewV7())}
	runtime, owner := testfixture.OpenIsolated(t, t, "dbtest_outbox")
	f.owner, f.backend = postgres.WrapPool(owner), postgres.WrapPool(runtime)
	var err error
	f.signer, err = commandauth.NewRandomSigner()
	testutil.NoError(t, err)
	at := time.Now().UTC()
	f.exec(t, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Outbox test',$2,$3,$4)`, f.workspace, f.workspace.String(), at, at)
	f.exec(t, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'outbox-test',$2,$3,$3)`, outboxIdentityID, outboxIdentityID.String(), at)
	f.exec(t, `INSERT INTO workspace_admins(workspace_id,identity_id,created_at) VALUES($1,$2,$3)`, f.workspace, outboxIdentityID, at)
	f.exec(t, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',$3)`, outboxSessionID, outboxIdentityID, at)
	return f
}

func (f *outboxFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.owner.Exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *outboxFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var count int
	if err := f.owner.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f *outboxFixture) node(t *testing.T) uuid.UUID {
	t.Helper()
	node := uuid.Must(uuid.NewV7())
	at := time.Now().UTC()
	f.exec(t, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',$4,$5)`, node, f.workspace, node.String(), at, at)
	endpoint := sha256.Sum256(node[:])
	f.exec(t, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',$3)`, node, endpoint[:], at)
	return node
}

func outboxRequest(node uuid.UUID) operations.CreateRequest {
	return operations.CreateRequest{
		NodeID: node, IdempotencyKey: uuid.NewString(), ExpectedVersion: 1,
		ActorID: outboxIdentityID.String(), ActorIdentityID: outboxIdentityID, ActorSessionID: outboxSessionID,
		Action: "operation.create", Reason: "outbox transaction test", TTL: time.Hour,
		RequestID: uuid.NewString(), Traceparent: "00-" + strings.Repeat("1", 32) + "-" + strings.Repeat("2", 16) + "-01",
		Payload: operations.SyntheticEchoPayload{Message: "Outbox transaction test"},
	}
}

func TestOutboxBackendIntegration(t *testing.T) {
	f := newOutboxFixture(t)
	ctx := context.Background()
	fault := &testfixture.CommitFaultBackend{Backend: f.backend, Err: lostCommit}
	service := operations.New(fault, 1, f.signer)
	request := outboxRequest(f.node(t))

	fault.Arm()
	if _, _, err := service.Create(ctx, request); !errors.Is(err, lostCommit) {
		t.Fatal(err)
	}
	for _, table := range []string{"operations", "commands", "outbox_events", "audit_events", "operation_events"} {
		if n := f.count(t, "SELECT count(*) FROM "+table); n != 0 {
			t.Fatal("partial intent", table, n)
		}
	}

	fault.CommitBeforeFail = true
	fault.Arm()
	created, replay, err := service.Create(ctx, request)
	if err != nil || replay {
		t.Fatal(created, replay, err)
	}
	again, replay, err := service.Create(ctx, request)
	if err != nil || !replay || again.ID != created.ID {
		t.Fatal("idempotent readback", again, replay, err)
	}

	request.Payload = operations.SyntheticEchoPayload{Message: "different effect"}
	if _, _, err := service.Create(ctx, request); !errors.Is(err, operations.ErrIdempotencyConflict) {
		t.Fatal(err)
	}

	fault.CommitBeforeFail = false
	fault.Arm()
	if jobs, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute); err == nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
	if n := f.count(t, "SELECT count(*) FROM command_attempts"); n != 0 {
		t.Fatal("partial claim", n)
	}

	fault.CommitBeforeFail = true
	fault.Arm()
	jobs, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatal("claim failed:", jobs, err)
	}
	if err := service.MarkSent(ctx, jobs[0]); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `SELECT count(*) FROM operation_events WHERE state='dispatched'`) != 1 {
		t.Fatal("duplicate dispatch projection")
	}

	f.exec(t, `UPDATE auth_sessions SET revoked_at=now() WHERE id=$1`, outboxSessionID)
	if _, _, err := service.Create(ctx, outboxRequest(f.node(t))); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("revoked session created operation: %v", err)
	}
}
