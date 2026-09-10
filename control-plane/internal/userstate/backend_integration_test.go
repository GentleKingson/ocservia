package userstate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/userstate/store"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func userStateBackend(t *testing.T) database.Backend {
	t.Helper()
	ctx := context.Background()
	if dsn := os.Getenv("PR02_DSN"); dsn != "" {
		options := mysql.Options{Engine: mysql.Engine(os.Getenv("PR02_ENGINE")), Environment: "test", DSN: dsn}
		admin, err := mysql.Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = admin.Close() })
		name := "pr02_user_state_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := admin.Exec(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP DATABASE `"+name+"`") })
		if _, err := admin.Exec(ctx, "GRANT ALL ON `"+name+"`.* TO 'ocservia_owner'@'%' WITH GRANT OPTION"); err != nil {
			t.Fatal(err)
		}
		cfg, err := driver.ParseDSN(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.DBName, cfg.User, cfg.Passwd = name, "ocservia_owner", "pr02-owner-test-only"
		options.DSN = cfg.FormatDSN()
		owner, err := mysql.Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = owner.Close() })
		if err := owner.Migrate(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if err := owner.GrantTestPrivileges(ctx); err != nil {
			t.Fatal(err)
		}
		cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
		options.DSN = cfg.FormatDSN()
		backend, err := mysql.Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = backend.Close() })
		return backend
	}
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("real PostgreSQL or PR02 backend required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return postgres.WrapPool(pool)
}

func TestUserStateBackendIntegration(t *testing.T) {
	b := userStateBackend(t)
	ctx := context.Background()
	_, my := b.(*mysql.Backend)
	query := func(pg, sql string, args ...any) (string, []any) {
		if my {
			pg = sql
			for i, v := range args {
				if id, ok := v.(uuid.UUID); ok {
					args[i] = mysql.UUIDBytes(id)
				}
			}
		}
		return pg, args
	}
	exec := func(pg, sql string, args ...any) {
		t.Helper()
		q, args := query(pg, sql, args...)
		if _, err := b.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	row := func(pg, sql string, args ...any) database.Row {
		q, args := query(pg, sql, args...)
		return b.QueryRow(ctx, q, args...)
	}
	workspace, node := uuid.New(), uuid.New()
	exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'desired',$2,now(),now())`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'desired',?,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, workspace, workspace.String())
	exec(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES($1,$2,'desired','offline',now(),now())`, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,'desired','offline',TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, node, workspace)
	for _, capability := range []string{"ocserv.users.write", "ocserv.groups.write"} {
		exec(`INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,$2,true)`, `INSERT INTO node_capabilities(node_id,capability,approved)VALUES(?,?,true)`, node, capability)
	}
	exec(`INSERT INTO node_sealing_keys(node_id,purpose,version,key_id,public_key_sha256,created_at)VALUES($1,1,1,'key',$2,now())`, `INSERT INTO node_sealing_keys(node_id,purpose,version,key_id,public_key_sha256,created_at)VALUES(?,1,1,'key',?,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, node, bytes.Repeat([]byte{1}, 32))
	signer := integrationCommandSigner()
	s := NewWithSignerBackend(b, signer)
	request := func(kind MutationKind, name string, version int64) MutationRequest {
		r := MutationRequest{NodeID: node, Kind: kind, Name: name, ExpectedVersion: version, Members: []string{}, IdempotencyKey: uuid.NewString(), TTL: time.Hour, ActorID: "operator", Reason: "PR02 desired", RequestID: uuid.NewString(), Traceparent: testTraceparent}
		if kind == UserCreate || kind == UserPasswordRotate {
			r.SealedPassword = &SealedSecret{Version: 1, Purpose: "user_password", KeyID: "key", Ciphertext: bytes.Repeat([]byte{1}, 32)}
		}
		return r
	}
	mutate := func(r MutationRequest) operationstore.Operation {
		t.Helper()
		op, replay, err := s.Mutate(ctx, r)
		if err != nil || replay {
			t.Fatalf("mutate %s: %+v %v %v", r.Kind, op, replay, err)
		}
		return op
	}
	assertEnvelope := func(op operationstore.Operation, expected, desired int64) {
		t.Helper()
		var encoded []byte
		var stored int64
		if err := row(`SELECT envelope,expected_version FROM commands WHERE id=$1`, `SELECT envelope,expected_version FROM commands WHERE id=?`, uuid.MustParse(*op.CommandID)).Scan(&encoded, &stored); err != nil {
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
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := commandauth.CanonicalV1(claims)
		if err != nil {
			t.Fatal(err)
		}
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
	secondRequest := request(GroupApply, "staff", 1)
	secondRequest.Members = []string{"carol"}
	second := mutate(secondRequest)
	assertEnvelope(second, 0, 1)
	var state string
	if err := row(`SELECT state FROM commands WHERE id=$1`, `SELECT state FROM commands WHERE id=?`, uuid.MustParse(*first.CommandID)).Scan(&state); err != nil || state != "superseded" {
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
	if err != nil {
		t.Fatal(err)
	}
	currentCommand := uuid.Nil
	if err := row(`SELECT command_id FROM operations WHERE id=$1`, `SELECT command_id FROM operations WHERE id=?`, uuid.MustParse(*states[0].OperationID)).Scan(&currentCommand); err != nil {
		t.Fatal(err)
	}
	lockedUntil, _ := value.FromTime(time.Now().UTC().Add(time.Minute))
	exec(`UPDATE outbox_events SET locked_by=$1,locked_until=$2 WHERE command_id=$3`, `UPDATE outbox_events SET locked_by=?,locked_until=? WHERE command_id=?`, uuid.New(), lockedUntil, currentCommand)
	fourthRequest := request(GroupApply, "staff", 3)
	fourthRequest.Members = []string{"member0"}
	fourth := mutate(fourthRequest)
	assertEnvelope(fourth, 1, 2)
	if err := row(`SELECT state FROM commands WHERE id=$1`, `SELECT state FROM commands WHERE id=?`, currentCommand).Scan(&state); err != nil || state != "queued" {
		t.Fatal("locked outbox superseded", state, err)
	}
	// A failed first create retries revision one, not an unapplied revision two.
	created := mutate(request(UserCreate, "alice", 0))
	assertEnvelope(created, 0, 1)
	exec(`UPDATE commands SET state='failed' WHERE id=$1`, `UPDATE commands SET state='failed' WHERE id=?`, uuid.MustParse(*created.CommandID))
	exec(`UPDATE operations SET state='failed' WHERE id=$1`, `UPDATE operations SET state='failed' WHERE id=?`, uuid.MustParse(created.ID))
	if _, _, err := s.Mutate(ctx, request(UserDisable, "alice", 1)); !errors.Is(err, ErrRevisionRecovery) {
		t.Fatal("wrong-kind recovery", err)
	}
	recovered := mutate(request(UserCreate, "alice", 1))
	assertEnvelope(recovered, 0, 1)
	// Failed fencing rolls back desired rows, supersession, outbox and audit.
	identity, err := coordination.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	leader, err := coordination.AcquireBackend(ctx, b, identity, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fenced := coordination.WithFence(ctx, leader)
	fencedRequest := request(GroupApply, "fenced", 0)
	if _, _, err := s.Mutate(fenced, fencedRequest); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE scheduler_leadership SET epoch=epoch+1 WHERE id=1`, `UPDATE scheduler_leadership SET epoch=epoch+1 WHERE id=1`)
	if _, _, err := s.Mutate(fenced, request(GroupApply, "fenced", 1)); !errors.Is(err, coordination.ErrNotLeader) {
		t.Fatal("fencing", err)
	}
	if _, _, err := s.Mutate(fenced, fencedRequest); !errors.Is(err, coordination.ErrNotLeader) {
		t.Fatal("fenced idempotent replay", err)
	}
	var version int64
	if err := row(`SELECT version FROM desired_groups WHERE node_id=$1 AND group_name='fenced'`, `SELECT version FROM desired_groups WHERE node_id=? AND group_name='fenced'`, node).Scan(&version); err != nil || version != 1 {
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
		// Identical UPDATE is a matched row, not a false not-found on MySQL.
		v := userstore.Desired{NodeID: node, Kind: "user_enable", Name: "alice", Version: 7, Revision: 3, Fingerprint: bytes.Repeat([]byte{2}, 32), At: infinity}
		if err := store.WriteDesired(ctx, v); err != nil {
			return err
		}
		return store.WriteDesired(ctx, v)
	})
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO observed_groups(node_id,group_name,members,revision,fingerprint,observed_at)VALUES($1,'logical',$2,3,$3,$4)`, `INSERT INTO observed_groups(node_id,group_name,members,revision,fingerprint,observed_at)VALUES(?,'logical',?,3,?,?)`, node, members, bytes.Repeat([]byte{2}, 32), infinity)
	object, err := value.ParseJSONB([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO node_observed_snapshots(node_id,observed_at,received_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,ocserv,system,path,last_heartbeat_at)VALUES($1,$2,$2,'desired',$3,'test','test','test',$4,$4,$4,$2)`, "INSERT INTO node_observed_snapshots(node_id,observed_at,received_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,ocserv,`system`,path,last_heartbeat_at)VALUES(?, ?, ?, 'desired', ?, 'test','test','test', ?, ?, ?, ?)", snapshotArgs(my, node, infinity, uuid.New(), object)...)
	exec(`INSERT INTO observed_groups(node_id,group_name,members,revision,fingerprint,observed_at)VALUES($1,'remote',$2,0,$3,$4)`, `INSERT INTO observed_groups(node_id,group_name,members,revision,fingerprint,observed_at)VALUES(?,'remote',?,0,?,?)`, node, value.TextList([]string{"Bob", "bob", "bob "}), bytes.Repeat([]byte{2}, 32), infinity)
	exec(`INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at)VALUES($1,'remote',true,0,$2,$3)`, `INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at)VALUES(?,'remote',true,0,?,?)`, node, bytes.Repeat([]byte{2}, 32), infinity)
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
	exec(`UPDATE node_observed_snapshots SET last_heartbeat_at=$1 WHERE node_id=$2`, `UPDATE node_observed_snapshots SET last_heartbeat_at=? WHERE node_id=?`, value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, node)
	assertCounts(1, 3, 4)
	states, err = s.List(ctx, node)
	if err != nil {
		t.Fatal(err)
	}
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

func snapshotArgs(my bool, node uuid.UUID, at value.Timestamp, instance uuid.UUID, object value.JSONB) []any {
	if my {
		return []any{node, at, at, instance, object, object, object, at}
	}
	return []any{node, at, instance, object}
}
