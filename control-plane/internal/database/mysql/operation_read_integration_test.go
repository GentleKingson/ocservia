package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func TestRealOperationReads(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	workspace, id, first, second := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	negative, positive := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	key := strings.Repeat("idempotency", 400) + " "
	hash := sha256.Sum256([]byte(key))
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'read',?,?,?)`, UUIDBytes(workspace), workspace.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	run(`INSERT INTO operations(id,workspace_id,state,request_id,idempotency_key,request_hash,created_at,updated_at,expires_at,completed_at)VALUES(?,?,'queued','read',?,?,?,?,?,?)`, UUIDBytes(id), UUIDBytes(workspace), key, hash[:], negative, positive, positive, negative)
	for i, event := range []uuid.UUID{first, second} {
		at := negative
		if i == 1 {
			at = positive
		}
		run(`INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES(?,?,'queued',?)`, UUIDBytes(event), UUIDBytes(id), at)
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
	op, err := service.Get(ctx, id)
	if err != nil || op.ID != id.String() || op.NodeID != nil || op.CommandID != nil || op.CreatedAt != negative || op.UpdatedAt != positive || op.ExpiresAt == nil || *op.ExpiresAt != positive {
		t.Fatal("logical operation", op, err)
	}
	if err := database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		got, same, err := store.FindIdempotent(ctx, workspace, key, hash[:])
		if err != nil {
			return err
		}
		if got.ID != op.ID || !same || got.CreatedAt != negative {
			t.Fatal("idempotent read", got, same)
		}
		got, _, err = store.FindIdempotent(ctx, workspace, strings.TrimSpace(key), hash[:])
		if err != nil {
			return err
		}
		if got.ID != "" {
			t.Fatal("trailing space matched", got)
		}
		_, same, err = store.FindIdempotent(ctx, workspace, key, make([]byte, 32))
		if err != nil {
			return err
		}
		if same {
			t.Fatal("conflicting hash matched")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	events, err := service.ListEvents(ctx, id, uuid.Nil, 10)
	if err != nil || len(events) != 2 || events[0].OccurredAt != negative || events[1].OccurredAt != positive {
		t.Fatal("logical events", events, err)
	}
	after, err := service.ListEvents(ctx, id, first, 10)
	if err != nil || len(after) != 1 || after[0].ID != second.String() {
		t.Fatal("event cursor", after, err)
	}
	sequence, found, err := service.EventSequence(ctx, id, first)
	if err != nil || !found || sequence != events[0].Sequence {
		t.Fatal("sequence", sequence, found, err)
	}
	if _, found, err := service.EventSequence(ctx, uuid.Must(uuid.NewV7()), first); err != nil || found {
		t.Fatal("cross-operation cursor", found, err)
	}
	run(`UPDATE operations SET expires_at=NULL WHERE id=?`, UUIDBytes(id))
	op, err = service.Get(ctx, id)
	if err != nil || op.ExpiresAt != nil {
		t.Fatal("NULL expiry", op, err)
	}
	if _, err := service.Get(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, database.ErrNotFound) {
		t.Fatal("missing operation", err)
	}
	reader := localslice.NewBackend(backend, nil)
	local, err := reader.GetOperation(ctx, id)
	if err != nil || local.CreatedAt != negative || local.UpdatedAt != positive || local.NodeID != nil || local.CommandID != nil {
		t.Fatal("local logical detail", local, err)
	}
	newer := uuid.Must(uuid.NewV7())
	run(`INSERT INTO operations(id,workspace_id,state,request_id,created_at,updated_at)VALUES(?,?,'unknown','read',?,?)`, UUIDBytes(newer), UUIDBytes(workspace), positive, negative)
	page, more, err := reader.ListOperationsInWorkspace(ctx, workspace, uuid.Nil, 1)
	if err != nil || !more || len(page) != 1 || page[0].ID != newer.String() || page[0].CreatedAt != positive {
		t.Fatal("local first page", page, more, err)
	}
	page, more, err = reader.ListOperationsInWorkspace(ctx, workspace, newer, 1)
	if err != nil || more || len(page) != 1 || page[0].ID != id.String() || page[0].CreatedAt != negative {
		t.Fatal("local cursor page", page, more, err)
	}
	page, more, err = reader.ListOperationsInWorkspace(ctx, uuid.New(), uuid.Nil, 1)
	if err != nil || more || len(page) != 0 {
		t.Fatal("local workspace isolation", page, more, err)
	}
	for _, scope := range []uuid.UUID{workspace, uuid.Nil} {
		summary, err := reader.OperationSummaryInWorkspace(ctx, scope)
		if err != nil || summary.Active != 1 || summary.Unknown != 1 {
			t.Fatal("local summary", summary, err)
		}
	}
}

func TestRealOperationIntentTransaction(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	workspace, node := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	if _, err := owner.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'intent',?,?,?)`, UUIDBytes(workspace), workspace.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,'intent','active',?,?)`, UUIDBytes(node), UUIDBytes(workspace), fixtureTimestamp(t, now), fixtureTimestamp(t, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved)VALUES(?,'service_reload',true)`, UUIDBytes(node)); err != nil {
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
	rejected := errors.New("reject intent transaction")
	for _, commit := range []bool{false, true} {
		id, command, outbox, event := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
		hash := sha256.Sum256([]byte(id.String()))
		err := database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
			store, err := operationstore.FromTransaction(tx)
			if err != nil {
				return err
			}
			locked, err := store.LockNode(ctx, node)
			if err != nil || locked.WorkspaceID != workspace || locked.Status != "active" || locked.Version != 1 {
				t.Fatal("operation node admission", locked, err)
			}
			for _, capability := range []string{"service_reload", "service_reload "} {
				approved, err := store.HasCapability(ctx, node, capability)
				if err != nil || approved != (capability == "service_reload") {
					t.Fatal("exact capability admission", capability, approved, err)
				}
			}
			if ready, err := store.AttestationReady(ctx, node); err != nil || ready {
				t.Fatal("unattested node admission", ready, err)
			}
			if present, err := store.HasSession(ctx, node, "absent", "boot"); err != nil || present {
				t.Fatal("unobserved session admission", present, err)
			}
			for _, ip := range []string{"192.0.2.1", "::ffff:192.0.2.1"} {
				if present, err := store.HasIPBan(ctx, node, ip); err != nil || present {
					t.Fatal("unobserved IP admission", ip, present, err)
				}
			}
			if err := store.InsertIntent(ctx, operationstore.QueuedIntent{ID: id, WorkspaceID: workspace, NodeID: node, CommandID: command, RequestID: id.String(), TraceID: strings.Repeat("1", 32), IdempotencyKey: id.String(), RequestHash: hash[:], CreatedAt: negative, ExpiresAt: positive}); err != nil {
				return err
			}
			if err := store.EnqueueCommand(ctx, operationstore.QueuedCommand{ID: command, OperationID: id, WorkspaceID: workspace, NodeID: node, OutboxID: outbox, EventID: event, PayloadType: "synthetic_noop", Envelope: []byte("x"), IdempotencyKey: id.String(), ExpectedVersion: 1, Traceparent: "00-" + strings.Repeat("1", 32) + "-" + strings.Repeat("2", 16) + "-01", CreatedAt: negative, ExpiresAt: positive, AvailableAt: positive}); err != nil {
				return err
			}
			if err := store.NotifyOutbox(ctx, outbox); err != nil {
				return err
			}
			if !commit {
				return rejected
			}
			return nil
		})
		if (!commit && !errors.Is(err, rejected)) || (commit && err != nil) {
			t.Fatal("intent transaction", commit, err)
		}
		for _, row := range []struct {
			table string
			id    uuid.UUID
		}{{"operations", id}, {"commands", command}, {"outbox_events", outbox}, {"operation_events", event}} {
			var count int
			if err := owner.QueryRow(ctx, `SELECT count(*) FROM `+row.table+` WHERE id=?`, UUIDBytes(row.id)).Scan(&count); err != nil || (count == 1) != commit {
				t.Fatal("atomic intent", row.table, count, err)
			}
		}
		if commit {
			var created, expires, available, published, locked value.Timestamp
			if err := owner.QueryRow(ctx, `SELECT c.created_at,c.expires_at,o.available_at,o.published_at,o.locked_until FROM commands c JOIN outbox_events o ON o.command_id=c.id WHERE c.id=?`, UUIDBytes(command)).Scan(&created, &expires, &available, &published, &locked); err != nil || created != negative || expires != positive || available != positive || published.Valid || locked.Valid {
				t.Fatal("logical queued command clocks", created, expires, available, published, locked, err)
			}
			service := operations.NewBackend(backend, 50, nil)
			assertQueued := func() {
				t.Helper()
				var commandState, operationState string
				if err := owner.QueryRow(ctx, `SELECT c.state,p.state,o.published_at FROM commands c JOIN operations p ON p.id=c.operation_id JOIN outbox_events o ON o.command_id=c.id WHERE c.id=?`, UUIDBytes(command)).Scan(&commandState, &operationState, &published); err != nil || commandState != "queued" || operationState != "queued" || published.Valid {
					t.Fatal("expiry changed protected command", commandState, operationState, published, err)
				}
			}
			if err := service.Expire(ctx); err != nil {
				t.Fatal(err)
			}
			assertQueued()
			err = database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
				store, err := operationstore.FromTransaction(tx)
				if err != nil {
					return err
				}
				if err := store.SupersedePending(ctx, node, "synthetic_noop", positive); err != nil {
					return err
				}
				got, err := store.Get(ctx, id)
				if err != nil || got.State != "superseded" || got.UpdatedAt != positive {
					t.Fatal("logical supersede", got, err)
				}
				return rejected
			})
			if !errors.Is(err, rejected) {
				t.Fatal("supersede rollback", err)
			}
			assertQueued()
			if _, err := owner.Exec(ctx, `UPDATE commands SET expires_at=? WHERE id=?`, fixtureTimestamp(t, now.Add(-time.Hour)), UUIDBytes(command)); err != nil {
				t.Fatal(err)
			}
			if _, err := owner.Exec(ctx, `INSERT INTO node_command_leases(node_id,command_id,lease_token,worker_id,leased_until,created_at)VALUES(?,?,?,?,?,?)`, UUIDBytes(node), UUIDBytes(command), UUIDBytes(uuid.New()), UUIDBytes(uuid.New()), fixtureTimestamp(t, now.Add(-time.Minute)), fixtureTimestamp(t, now.Add(-time.Hour))); err != nil {
				t.Fatal(err)
			}
			if err := service.Expire(ctx); err != nil {
				t.Fatal(err)
			}
			assertQueued()
			if err := database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
				store, err := operationstore.FromTransaction(tx)
				if err != nil {
					return err
				}
				return store.SupersedePending(ctx, node, "synthetic_noop", positive)
			}); err != nil {
				t.Fatal(err)
			}
			assertQueued()
			if _, err := owner.Exec(ctx, `DELETE FROM node_command_leases WHERE command_id=?`, UUIDBytes(command)); err != nil {
				t.Fatal(err)
			}
			err = database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
				store, err := operationstore.FromTransaction(tx)
				if err != nil {
					return err
				}
				if err := store.ExpireQueued(ctx); err != nil {
					return err
				}
				return rejected
			})
			if !errors.Is(err, rejected) {
				t.Fatal("expiry rollback", err)
			}
			assertQueued()
			for range 2 {
				if err := service.Expire(ctx); err != nil {
					t.Fatal(err)
				}
			}
			operation, err := service.Get(ctx, id)
			if err != nil || operation.State != "expired" || operation.Version != 2 {
				t.Fatal("expired operation", operation, err)
			}
			var eventCount int
			if err := owner.QueryRow(ctx, `SELECT count(*) FROM operation_events WHERE operation_id=? AND state='expired'`, UUIDBytes(id)).Scan(&eventCount); err != nil || eventCount != 1 {
				t.Fatal("duplicate or missing expiry event", eventCount, err)
			}
		}
	}
	signer, err := commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	service := operations.NewBackend(backend, 50, signer)
	request := operations.CreateRequest{NodeID: node, IdempotencyKey: uuid.NewString(), ExpectedVersion: 1, Kind: operations.SyntheticEcho, Message: "portable", TTL: time.Minute, RequestID: uuid.NewString(), Traceparent: "00-" + strings.Repeat("1", 32) + "-" + strings.Repeat("2", 16) + "-01"}
	created, replayed, err := service.CreateSynthetic(ctx, request)
	if err != nil || replayed || created.State != "queued" || created.CommandID == nil {
		t.Fatal("common operation creation", created, replayed, err)
	}
	replay, replayed, err := service.CreateSynthetic(ctx, request)
	if err != nil || !replayed || replay.ID != created.ID {
		t.Fatal("common operation replay", replay, replayed, err)
	}
	conflict := request
	conflict.Message = "different"
	if _, _, err := service.CreateSynthetic(ctx, conflict); !errors.Is(err, operations.ErrIdempotencyConflict) {
		t.Fatal("common operation conflict", err)
	}
	operationID := uuid.MustParse(created.ID)
	var encoded []byte
	var audits int
	if err := owner.QueryRow(ctx, `SELECT envelope FROM commands WHERE operation_id=?`, UUIDBytes(operationID)).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(encoded, &envelope); err != nil || envelope.GetSyntheticEcho().GetMessage() != request.Message || !bytes.Equal(envelope.GetOperationId(), operationID[:]) || envelope.Authorization == nil {
		t.Fatal("typed common command", &envelope, err)
	}
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_id=? AND result='intent'`, UUIDBytes(operationID)).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("common intent audit", audits, err)
	}
	request.IdempotencyKey = uuid.NewString()
	request.SupersedePending = true
	if _, _, err := service.CreateSynthetic(ctx, request); err != nil {
		t.Fatal("common superseding creation", err)
	}
	superseded, err := service.Get(ctx, operationID)
	if err != nil || superseded.State != "superseded" {
		t.Fatal("common superseding state", superseded, err)
	}
}
