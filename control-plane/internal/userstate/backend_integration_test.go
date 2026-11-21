package userstate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/userstate/store"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func userStateBackend(t *testing.T) database.Backend {
	t.Helper()
	pool := testfixture.OpenRuntimePool(t)
	// PostgreSQL packages share a database. Preserve the fencing epoch, but
	// leave the singleton expired both before and after this fixture.
	expireLeadership := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, `UPDATE scheduler_leadership SET lease_until='-infinity' WHERE id=1`); err != nil {
			t.Fatalf("expire fixture scheduler leadership: %v", err)
		}
	}
	t.Cleanup(expireLeadership)
	expireLeadership()
	return postgres.WrapPool(pool)
}

func TestUserStateBackendIntegration(t *testing.T) {
	b := userStateBackend(t)
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := b.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	row := func(query string, args ...any) database.Row {
		return b.QueryRow(ctx, query, args...)
	}
	workspace, node, identity, session := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'desired',$2,now(),now())`, workspace, workspace.String())
	exec(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES($1,$2,'desired','offline',now(),now())`, node, workspace)
	exec(`INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES($1,'userstate-test',$2,now(),now())`, identity, identity.String())
	exec(`INSERT INTO workspace_admins(workspace_id,identity_id,created_at)VALUES($1,$2,now())`, workspace, identity)
	exec(`INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES($1,$2,now()+interval '1 hour',now())`, session, identity)
	for _, capability := range []string{"ocserv.users.write", "ocserv.groups.write"} {
		exec(`INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,$2,true)`, node, capability)
	}
	exec(`INSERT INTO node_sealing_keys(node_id,purpose,version,key_id,public_key_sha256,created_at)VALUES($1,1,1,'key',$2,now())`, node, bytes.Repeat([]byte{1}, 32))
	signer := testfixture.CommandSigner(2)
	s := NewWithSignerBackend(b, signer)
	request := func(kind MutationKind, name string, version int64) MutationRequest {
		r := MutationRequest{NodeID: node, ActorIdentityID: identity, ActorSessionID: session, Kind: kind, Name: name, ExpectedVersion: version, Members: []string{}, IdempotencyKey: uuid.NewString(), TTL: time.Hour, ActorID: "operator", Reason: "Database backend desired state", RequestID: uuid.NewString(), Traceparent: testTraceparent}
		if kind == UserCreate || kind == UserPasswordRotate {
			r.SealedPassword = &SealedSecret{Version: 1, Purpose: "user_password", KeyID: "key", Ciphertext: bytes.Repeat([]byte{1}, 32)}
		}
		return r
	}
	mutate := func(r MutationRequest) operations.Operation {
		t.Helper()
		op, replay, err := s.Mutate(ctx, r)
		if err != nil || replay {
			t.Fatalf("mutate %s: %+v %v %v", r.Kind, op, replay, err)
		}
		return op
	}
	assertEnvelope := func(op operations.Operation, expected, desired int64) {
		t.Helper()
		var encoded []byte
		var stored int64
		if err := row(`SELECT envelope,expected_version FROM commands WHERE id=$1`, uuid.MustParse(*op.CommandID)).Scan(&encoded, &stored); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(encoded, &envelope); err != nil {
			t.Fatal(err)
		}
		got := uint64(0)
		switch p := envelope.Payload.(type) {
		case *agentv1.CommandEnvelope_GroupApply:
			got = p.GroupApply.DesiredRevision
		case *agentv1.CommandEnvelope_UserCreate:
			got = p.UserCreate.DesiredRevision
		case *agentv1.CommandEnvelope_UserPasswordRotate:
			got = p.UserPasswordRotate.DesiredRevision
		}
		claims, err := commandauth.ClaimsFromEnvelopeV1(&envelope)
		testutil.NoError(t, err)
		canonical, err := commandauth.CanonicalV1(claims)
		testutil.NoError(t, err)
		if !ed25519.Verify(signer.PublicKey(), canonical, envelope.GetAuthorization().GetSignature()) || stored != expected || got != uint64(desired) || envelope.ExpectedRevision != 1 {
			t.Fatal("signed revision", stored, got, envelope.ExpectedRevision)
		}
	}
	firstRequest := request(GroupApply, "staff", 0)
	firstRequest.Members = []string{"bob", "alice", "bob"}
	first := mutate(firstRequest)
	assertEnvelope(first, 0, 1)
	replay, wasReplay, err := s.Mutate(ctx, firstRequest)
	if err != nil || !wasReplay || replay.ID != first.ID {
		t.Fatal("replay", replay, wasReplay, err)
	}
	changed := firstRequest
	changed.Members = []string{"other"}
	if _, _, err := s.Mutate(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatal("reused key", err)
	}
	denied := request(GroupApply, "revoked-session", 0)
	denied.Members = []string{"alice"}
	exec(`UPDATE auth_sessions SET revoked_at=now() WHERE id=$1`, session)
	if _, _, err := s.Mutate(ctx, denied); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("revoked session mutation", err)
	}
	var deniedRows int
	if err := row(`SELECT count(*) FROM desired_groups WHERE node_id=$1 AND group_name=$2`, node, denied.Name).Scan(&deniedRows); err != nil || deniedRows != 0 {
		t.Fatal("revoked session changed desired state", deniedRows, err)
	}
	if err := row(`SELECT count(*) FROM operations WHERE workspace_id=$1 AND idempotency_key=$2`, workspace, denied.IdempotencyKey).Scan(&deniedRows); err != nil || deniedRows != 0 {
		t.Fatal("revoked session enqueued operation", deniedRows, err)
	}
	if err := row(`SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND request_id=$2`, workspace, denied.RequestID).Scan(&deniedRows); err != nil || deniedRows != 0 {
		t.Fatal("revoked session appended audit", deniedRows, err)
	}
	exec(`UPDATE auth_sessions SET revoked_at=NULL WHERE id=$1`, session)
	secondRequest := request(GroupApply, "staff", 1)
	secondRequest.Members = []string{"carol"}
	second := mutate(secondRequest)
	assertEnvelope(second, 0, 1)
	var state string
	if err := row(`SELECT state FROM commands WHERE id=$1`, uuid.MustParse(*first.CommandID)).Scan(&state); err != nil || state != "superseded" {
		t.Fatal("coalescing", state, err)
	}
	states, err := s.List(ctx, node)
	if err != nil || len(states) != 1 || states[0].Convergence != "offline_pending" || *states[0].DesiredVersion != 2 || *states[0].DesiredRevision != 1 {
		t.Fatal("list", states, err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := request(GroupApply, "staff", 2)
			r.Members = []string{fmt.Sprintf("member%d", i)}
			_, _, err := s.Mutate(ctx, r)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrVersionConflict) {
			t.Fatal("concurrent mutation", err)
		}
	}
	if successes != 1 {
		t.Fatal("optimistic version winners", successes)
	}
	// A locked outbox must not be coalesced. The next command follows it.
	states, err = s.List(ctx, node)
	testutil.NoError(t, err)
	currentCommand := uuid.Nil
	if err := row(`SELECT command_id FROM operations WHERE id=$1`, uuid.MustParse(*states[0].OperationID)).Scan(&currentCommand); err != nil {
		t.Fatal(err)
	}
	lockedUntil, _ := value.FromTime(time.Now().UTC().Add(time.Minute))
	exec(`UPDATE outbox_events SET locked_by=$1,locked_until=$2 WHERE command_id=$3`, uuid.New(), lockedUntil, currentCommand)
	fourthRequest := request(GroupApply, "staff", 3)
	fourthRequest.Members = []string{"member0"}
	fourth := mutate(fourthRequest)
	assertEnvelope(fourth, 1, 2)
	if err := row(`SELECT state FROM commands WHERE id=$1`, currentCommand).Scan(&state); err != nil || state != "queued" {
		t.Fatal("locked outbox superseded", state, err)
	}
	// A failed first create retries revision one, not an unapplied revision two.
	created := mutate(request(UserCreate, "alice", 0))
	assertEnvelope(created, 0, 1)
	exec(`UPDATE commands SET state='failed' WHERE id=$1`, uuid.MustParse(*created.CommandID))
	exec(`UPDATE operations SET state='failed' WHERE id=$1`, uuid.MustParse(created.ID))
	if _, _, err := s.Mutate(ctx, request(UserDisable, "alice", 1)); !errors.Is(err, ErrRevisionRecovery) {
		t.Fatal("wrong-kind recovery", err)
	}
	recovered := mutate(request(UserCreate, "alice", 1))
	assertEnvelope(recovered, 0, 1)
	// Failed fencing rolls back desired rows, supersession, outbox and audit.
	schedulerIdentity, err := coordination.NewIdentity()
	testutil.NoError(t, err)
	leader, err := coordination.AcquireBackend(ctx, b, schedulerIdentity, time.Minute)
	testutil.NoError(t, err)
	fenced := coordination.WithFence(ctx, leader)
	fencedRequest := request(GroupApply, "fenced", 0)
	if _, _, err := s.Mutate(fenced, fencedRequest); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE scheduler_leadership SET epoch=epoch+1 WHERE id=1`)
	if _, _, err := s.Mutate(fenced, request(GroupApply, "fenced", 1)); !errors.Is(err, coordination.ErrNotLeader) {
		t.Fatal("fencing", err)
	}
	if _, _, err := s.Mutate(fenced, fencedRequest); !errors.Is(err, coordination.ErrNotLeader) {
		t.Fatal("fenced idempotent replay", err)
	}
	var version int64
	if err := row(`SELECT version FROM desired_groups WHERE node_id=$1 AND group_name='fenced'`, node).Scan(&version); err != nil || version != 1 {
		t.Fatal("fenced rollback", version, err)
	}
	overflow := request(GroupApply, "overflow", 0)
	for i := range MaxManagedResources {
		overflow.Members = append(overflow.Members, fmt.Sprintf("capacity%d", i))
	}
	if _, _, err := s.Mutate(ctx, overflow); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatal("membership capacity", err)
	}
	// Store and read contracts retain NULL, dimensions, bounds and infinity.
	members := value.TextList([]string{"alpha", "alpha", "", "long " + strings.Repeat("x", 500)})
	members.Dimensions = []value.Dimension{{Length: 2, LowerBound: -1}, {Length: 2, LowerBound: 5}}
	members.Elements[2] = nil
	infinity := value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	err = database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		store, err := userstore.From(tx)
		if err != nil {
			return err
		}
		if err := store.WriteDesired(ctx, userstore.Desired{NodeID: node, Kind: "group_apply", Name: "logical", Version: 7, Revision: 3, Fingerprint: bytes.Repeat([]byte{2}, 32), Members: members, At: infinity}); err != nil {
			return err
		}
		// Identical UPDATE is a matched row, not a not-found error.
		v := userstore.Desired{NodeID: node, Kind: "user_enable", Name: "alice", Version: 7, Revision: 3, Fingerprint: bytes.Repeat([]byte{2}, 32), At: infinity}
		if err := store.WriteDesired(ctx, v); err != nil {
			return err
		}
		return store.WriteDesired(ctx, v)
	})
	testutil.NoError(t, err)
	exec(`INSERT INTO observed_groups(node_id,group_name,members,revision,fingerprint,observed_at)VALUES($1,'logical',$2,3,$3,$4)`, node, members, bytes.Repeat([]byte{2}, 32), infinity)
	object, err := value.ParseJSONB([]byte(`{}`))
	testutil.NoError(t, err)
	exec(`INSERT INTO node_observed_snapshots(node_id,observed_at,received_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,ocserv,system,path,last_heartbeat_at)VALUES($1,$2,$2,'desired',$3,'test','test','test',$4,$4,$4,$2)`, node, infinity, uuid.New(), object)
	exec(`INSERT INTO observed_groups(node_id,group_name,members,revision,fingerprint,observed_at)VALUES($1,'remote',$2,0,$3,$4)`, node, value.TextList([]string{"Bob", "bob", "bob "}), bytes.Repeat([]byte{2}, 32), infinity)
	exec(`INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at)VALUES($1,'remote',true,0,$2,$3)`, node, bytes.Repeat([]byte{2}, 32), infinity)
	assertCounts := func(users, groups, memberships int) {
		t.Helper()
		if err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
			store, err := userstore.From(tx)
			if err != nil {
				return err
			}
			fresh, err := value.FromTime(time.Now().UTC().Add(-90 * time.Second))
			if err != nil {
				return err
			}
			u, err := store.CountResources(ctx, node, "absent", false, fresh)
			if err != nil {
				return err
			}
			g, err := store.CountResources(ctx, node, "absent", true, fresh)
			if err != nil {
				return err
			}
			m, err := store.CountMemberships(ctx, node, "absent", fresh)
			if err != nil {
				return err
			}
			if u != users || g != groups || m != memberships {
				return fmt.Errorf("counts: %d/%d/%d want %d/%d/%d", u, g, m, users, groups, memberships)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertCounts(2, 4, 7)
	exec(`UPDATE node_observed_snapshots SET last_heartbeat_at=$1 WHERE node_id=$2`, value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, node)
	assertCounts(1, 3, 4)
	states, err = s.List(ctx, node)
	testutil.NoError(t, err)
	found := false
	for _, item := range states {
		if item.Name != "logical" {
			continue
		}
		found = true
		if !reflect.DeepEqual(item.DesiredMemberDimensions, members.Dimensions) || !reflect.DeepEqual(item.DesiredMembers, members.Elements) || !reflect.DeepEqual(item.ObservedMembers, members.Elements) || item.ObservedAt == nil || *item.ObservedAt != infinity || item.Convergence != "converged" {
			t.Fatal("lossless resource", item)
		}
		encoded, err := json.Marshal(item)
		if err != nil || !bytes.Contains(encoded, []byte(`"observed_at":"infinity"`)) || !bytes.Contains(encoded, []byte(`null`)) {
			t.Fatal("wire values", string(encoded), err)
		}
	}
	if !found {
		t.Fatal("missing logical resource")
	}
	if _, err := audit.NewBackendManager(b, nil).Verify(ctx, workspace); err != nil {
		t.Fatal("audit chain", err)
	}
}
