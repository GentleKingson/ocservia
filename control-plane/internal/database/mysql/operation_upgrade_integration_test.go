package mysql

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func TestRealUpgradeReconciliation(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	workspace := uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	stamp := fixtureTimestamp(t, now)
	negative, positive := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	if _, err := owner.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'upgrade',?,?,?)`, UUIDBytes(workspace), workspace.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now)); err != nil {
		t.Fatal(err)
	}
	if err := owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = cfg.FormatDSN()
	backend, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	service := operations.NewBackend(backend, 50, nil)
	for _, test := range []struct {
		name, durable, version, want, command string
		scheduled, heartbeat                  value.Timestamp
		rejectAudit                           bool
	}{
		{"positive infinity deadline", "", "1.2.0", "accepted", "accepted", positive, positive, false},
		{"negative infinity deadline", "", "1.2.0", "unknown", "expired", negative, positive, true},
		{"infinite fresh observation", "succeeded", "2.0.0", "succeeded", "succeeded", stamp, positive, false},
		{"negative infinity heartbeat", "succeeded", "2.0.0", "accepted", "accepted", stamp, negative, false},
		{"reconnect is progress", "", "1.2.0", "running", "accepted", stamp, positive, false},
		{"durable failure while stale", "failed", "1.2.0", "failed", "failed", stamp, negative, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := func(query string, args ...any) {
				t.Helper()
				if _, err := owner.Exec(ctx, query, args...); err != nil {
					t.Fatal(err)
				}
			}
			node, id, command, outbox, event := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
			run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,?,'active',?,?)`, UUIDBytes(node), UUIDBytes(workspace), node.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now))
			run("INSERT INTO node_observed_snapshots(node_id,observed_at,received_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,`system`,path,last_heartbeat_at)VALUES(?,?,?,'boot',?,?,'1.3.0','test','amd64','{}','{}','{}',?)", UUIDBytes(node), positive, stamp, UUIDBytes(uuid.New()), test.version, test.heartbeat)
			err := database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
				store, err := operationstore.FromTransaction(tx)
				if err != nil {
					return err
				}
				if err := store.InsertIntent(ctx, operationstore.QueuedIntent{ID: id, WorkspaceID: workspace, NodeID: node, CommandID: command, RequestID: id.String(), IdempotencyKey: id.String(), RequestHash: bytes.Repeat([]byte{1}, 32), CreatedAt: negative, ExpiresAt: positive}); err != nil {
					return err
				}
				if err := store.EnqueueCommand(ctx, operationstore.QueuedCommand{ID: command, OperationID: id, WorkspaceID: workspace, NodeID: node, OutboxID: outbox, EventID: event, PayloadType: "agent_upgrade", IdempotencyKey: id.String(), Envelope: []byte("fixture"), ExpectedVersion: 1, Traceparent: "00-11111111111111111111111111111111-2222222222222222-01", CreatedAt: negative, ExpiresAt: positive, AvailableAt: stamp}); err != nil {
					return err
				}
				return store.InsertUpgrade(ctx, operationstore.PendingUpgrade{ID: id, WorkspaceID: workspace, NodeID: node, TargetVersion: "2.0.0", FromVersion: "1.2.0", Architecture: "amd64", PackageSHA256: bytes.Repeat([]byte{1}, 32), CreatedAt: negative})
			})
			if err != nil {
				t.Fatal(err)
			}
			run(`UPDATE operations SET state='accepted' WHERE id=?`, UUIDBytes(id))
			run(`UPDATE commands SET state='accepted' WHERE id=?`, UUIDBytes(command))
			run(`UPDATE agent_upgrade_operations SET state='accepted',scheduled_at=? WHERE operation_id=?`, test.scheduled, UUIDBytes(id))
			if test.durable != "" {
				run(`INSERT INTO node_agent_upgrade_results(operation_id,node_id,state,target_version,detail,completed_at,reported_at,privileged_result_proof)VALUES(?,?,?,'2.0.0','',?,?,?)`, UUIDBytes(id), UUIDBytes(node), test.durable, positive, positive, []byte("verified fixture"))
			}
			if test.rejectAudit {
				run(`CREATE TRIGGER upgrade_audit_failure BEFORE INSERT ON audit_events FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='isolated audit failure'`)
				err := service.ReconcileAgentUpgrades(ctx)
				run(`DROP TRIGGER upgrade_audit_failure`)
				if err == nil {
					t.Fatal("audit failure committed upgrade")
				}
				var state, projection string
				var published value.Timestamp
				if err := owner.QueryRow(ctx, `SELECT o.state,u.state,b.published_at FROM operations o JOIN agent_upgrade_operations u ON u.operation_id=o.id JOIN outbox_events b ON b.command_id=o.command_id WHERE o.id=?`, UUIDBytes(id)).Scan(&state, &projection, &published); err != nil || state != "accepted" || projection != "accepted" || published.Valid {
					t.Fatal("upgrade audit rollback", state, projection, published, err)
				}
			}
			for range 2 {
				if err := service.ReconcileAgentUpgrades(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var state, commandState, projection string
			var completed, published value.Timestamp
			if err := owner.QueryRow(ctx, `SELECT o.state,c.state,u.state,u.completed_at,b.published_at FROM operations o JOIN commands c ON c.operation_id=o.id JOIN agent_upgrade_operations u ON u.operation_id=o.id JOIN outbox_events b ON b.command_id=c.id WHERE o.id=?`, UUIDBytes(id)).Scan(&state, &commandState, &projection, &completed, &published); err != nil || state != test.want || projection != test.want || commandState != test.command {
				t.Fatal("upgrade outcome", state, commandState, projection, err)
			}
			terminal := test.want == "unknown" || test.want == "succeeded" || test.want == "failed"
			if completed.Valid != terminal || published.Valid != terminal {
				t.Fatal("upgrade completion clocks", completed, published)
			}
			if terminal {
				var count int
				if err := owner.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_id=? AND action='agent.upgrade'`, UUIDBytes(id)).Scan(&count); err != nil || count != 1 {
					t.Fatal("duplicate terminal audit", count, err)
				}
			}
		})
	}
}
