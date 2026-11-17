package postgres

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
	"github.com/google/uuid"
)

func TestObservedStateTransactionsIntegration(t *testing.T) {
	b := testBackend(t)
	ctx := context.Background()
	workspace, node := uuid.New(), uuid.New()
	if _, err := b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'types',$2,now(),now())`, workspace, workspace.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'types','active',now(),now())`, node, workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b.Exec(ctx, `DELETE FROM telemetry_security_events WHERE node_id=$1`, node)
		b.Exec(ctx, `DELETE FROM observed_groups WHERE node_id=$1`, node)
		b.Exec(ctx, `DELETE FROM nodes WHERE id=$1`, node)
		b.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, workspace)
	})
	text, blank := "member", ""
	group := telemetrystore.Group{Name: "staff", Members: value.TextArray{Valid: true, Dimensions: []value.Dimension{{Length: 2, LowerBound: -2}, {Length: 2, LowerBound: 5}}, Elements: []*string{&text, nil, &blank, &text}}, Revision: 3, Fingerprint: make([]byte, 32), ObservedAt: value.Timestamp{Valid: true, Micros: value.EndTimestamp - 1}}
	detail, err := value.ParseJSONB([]byte(`{"decimal":1e1000,"tiny":1e-1000,"exact":9007199254740993,"last":1,"last":2}`))
	if err != nil {
		t.Fatal(err)
	}
	event := telemetrystore.SecurityEvent{ID: uuid.New(), ObservedAt: value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, Severity: "info", Type: "domain-parity", Detail: detail}
	write := func(after error) error {
		return database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
			s, err := telemetrystore.FromTransaction(tx)
			if err != nil {
				return err
			}
			if err := s.LockNode(ctx, node); err != nil {
				return err
			}
			if err := s.ReplaceGroups(ctx, node, []telemetrystore.Group{group}); err != nil {
				return err
			}
			if err := s.InsertSecurity(ctx, node, event); err != nil {
				return err
			}
			return after
		})
	}
	if err = write(nil); err != nil {
		t.Fatal(err)
	}
	if err = write(nil); err != nil {
		t.Fatal("idempotent event replay", err)
	}
	check := func() {
		t.Helper()
		err := database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
			s, err := telemetrystore.FromTransaction(tx)
			if err != nil {
				return err
			}
			groups, err := s.Groups(ctx, node)
			if err != nil {
				return err
			}
			if len(groups) != 1 || groups[0].ObservedAt != group.ObservedAt || groups[0].Revision != 3 {
				t.Fatal("group changed", groups)
			}
			want, _ := group.Members.Bytes()
			got, _ := groups[0].Members.Bytes()
			if !bytes.Equal(want, got) {
				t.Fatal("array dimensions/NULL changed")
			}
			stored, err := s.SecurityEvent(ctx, event.ID)
			if err != nil {
				return err
			}
			if stored.ObservedAt != event.ObservedAt || !bytes.Equal(stored.Detail.Bytes(), event.Detail.Bytes()) {
				t.Fatal("event timestamp/JSON changed")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	check()
	marker := errors.New("rollback after both domain writes")
	group.Revision = 4
	if err = write(marker); !errors.Is(err, marker) {
		t.Fatal(err)
	}
	group.Revision = 3
	check()
	if err = database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		s, _ := telemetrystore.FromTransaction(tx)
		return s.InsertSecurity(ctx, uuid.New(), telemetrystore.SecurityEvent{ID: uuid.New(), ObservedAt: event.ObservedAt, Severity: event.Severity, Type: event.Type, Detail: event.Detail})
	}); !errors.Is(err, database.ErrForeignKey) {
		t.Fatal("foreign key not enforced", err)
	}
}
