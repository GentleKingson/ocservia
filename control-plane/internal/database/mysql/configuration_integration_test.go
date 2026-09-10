package mysql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	configurationstore "github.com/GentleKingson/ocservia/control-plane/internal/configplan/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func TestRealConfigurationReadAndIntent(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	workspace, node, id, command := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	if _, err := owner.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'config',?,?,?)`, UUIDBytes(workspace), workspace.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,'config','active',?,?)`, UUIDBytes(node), UUIDBytes(workspace), fixtureTimestamp(t, now), fixtureTimestamp(t, now)); err != nil {
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
	negative, positive := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	warnings, err := value.ParseJSONB([]byte(`["safe",null,[1.234567890123456789],{"value":true}]`))
	if err != nil {
		t.Fatal(err)
	}
	err = database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		operations, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		if _, err := operations.LockNode(ctx, node); err != nil {
			return err
		}
		if err := operations.InsertIntent(ctx, operationstore.QueuedIntent{ID: id, WorkspaceID: workspace, NodeID: node, CommandID: command, RequestID: id.String(), IdempotencyKey: id.String(), RequestHash: bytes.Repeat([]byte{1}, 32), CreatedAt: negative, ExpiresAt: positive}); err != nil {
			return err
		}
		store, err := configurationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		if err := store.InsertPlan(ctx, configurationstore.PendingPlan{ID: id, WorkspaceID: workspace, NodeID: node, TemplateName: "test", CandidateHash: bytes.Repeat([]byte{1}, 32), CandidateRedacted: "safe", Warnings: warnings, CreatedAt: negative, ExpiresAt: positive}); err != nil {
			return err
		}
		for _, revision := range []uint64{1, 3, 2} {
			advanced, err := store.AdvanceDesiredRevision(ctx, node, revision, positive)
			if err != nil || advanced != (revision != 3) {
				t.Fatal("desired revision fence", revision, advanced, err)
			}
		}
		state, err := store.State(ctx, node)
		if err != nil || state.Revision != 0 || state.DesiredRevision != 2 || state.Locked {
			t.Fatal("configuration state", state, err)
		}
		proof, err := store.Proof(ctx, id)
		if err != nil || proof.WorkspaceID != workspace || proof.NodeID != node || proof.ExpiresAt != positive || proof.State != "queued" {
			t.Fatal("configuration proof", proof, err)
		}
		if active, err := store.HasActiveApply(ctx, node); err != nil || active {
			t.Fatal("active apply", active, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	service := configplan.NewBackend(backend, nil)
	plan, err := service.Get(ctx, id)
	if err != nil || plan.ExpiresAt != positive || plan.CreatedAt != negative || plan.Validation != "pending" || plan.ApprovalID != nil {
		t.Fatal("logical configuration plan", plan, err)
	}
	encoded, err := json.Marshal(plan.Warnings)
	if err != nil {
		t.Fatal(err)
	}
	got, err := value.ParseJSONB(encoded)
	if err != nil || !bytes.Equal(got.Bytes(), warnings.Bytes()) {
		t.Fatal("configuration warnings narrowed", string(encoded), err)
	}
	gotWorkspace, gotNode, err := service.Resource(ctx, id)
	if err != nil || gotWorkspace != workspace || gotNode != node {
		t.Fatal("configuration resource", gotWorkspace, gotNode, err)
	}
	if _, err := service.Get(ctx, uuid.New()); !configplan.IsNotFound(err) {
		t.Fatal("missing plan", err)
	}
	rejected := errors.New("rollback configuration revision")
	err = database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := configurationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		if advanced, err := store.AdvanceDesiredRevision(ctx, node, 3, negative); err != nil || !advanced {
			t.Fatal("advance before rollback", advanced, err)
		}
		return rejected
	})
	if !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	var revision int64
	var updated value.Timestamp
	if err := owner.QueryRow(ctx, `SELECT desired_revision,updated_at FROM node_config_state WHERE node_id=?`, UUIDBytes(node)).Scan(&revision, &updated); err != nil || revision != 2 || updated != positive {
		t.Fatal("revision rollback", revision, updated, err)
	}
}
