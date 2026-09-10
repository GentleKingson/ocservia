package mysql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/semantictest"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func TestRealCommandLimits(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	if err := owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	config, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	config.User, config.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = config.FormatDSN()
	b, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	now := time.Now().UTC().Truncate(time.Microsecond)
	node := func(t *testing.T, workspace uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := b.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,?,'active',?,?)`, UUIDBytes(id), UUIDBytes(workspace), id.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	semantictest.CommandLimits(t, semantictest.CommandLimitHarness{
		Backend: b,
		Scope: func(t *testing.T) (uuid.UUID, uuid.UUID) {
			workspace := uuid.New()
			if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'command-limit',?,?,?)`, UUIDBytes(workspace), workspace.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now)); err != nil {
				t.Fatal(err)
			}
			return workspace, node(t, workspace)
		},
		Node: node,
		Queue: func(ctx context.Context, tx database.Tx, workspace, node uuid.UUID, count int, state string) error {
			var nodeValue any
			if node != uuid.Nil {
				nodeValue = UUIDBytes(node)
			}
			for range count {
				if _, err := tx.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,state,request_id,created_at,updated_at) VALUES(?,?,?,?,'command-limit',?,?)`, UUIDBytes(uuid.New()), UUIDBytes(workspace), nodeValue, state, fixtureTimestamp(t, now), fixtureTimestamp(t, now)); err != nil {
					return err
				}
			}
			return nil
		},
		Command: func(ctx context.Context, tx database.Tx, workspace, node uuid.UUID, state string, lease bool) error {
			operation, command := uuid.New(), uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,state,request_id,created_at,updated_at) VALUES(?,?,?,?,'command-limit',?,?)`, UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), state, fixtureTimestamp(t, now), fixtureTimestamp(t, now)); err != nil {
				return err
			}
			trace := "00-" + strings.Repeat("a", 32) + "-" + strings.Repeat("b", 16) + "-01"
			if _, err := tx.Exec(ctx, `INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,traceparent,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,'synthetic_noop','x',?,1,?,?,?,?)`, UUIDBytes(command), UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), state, command.String(), trace, fixtureTimestamp(t, now.Add(time.Minute)), fixtureTimestamp(t, now), fixtureTimestamp(t, now)); err != nil {
				return err
			}
			if lease {
				_, err := tx.Exec(ctx, `INSERT INTO node_command_leases(node_id,command_id,lease_token,worker_id,leased_until,created_at) VALUES(?,?,?,?,?,?)`, UUIDBytes(node), UUIDBytes(command), UUIDBytes(uuid.New()), UUIDBytes(uuid.New()), fixtureTimestamp(t, now.Add(time.Minute)), fixtureTimestamp(t, now))
				return err
			}
			return nil
		},
	})
}
