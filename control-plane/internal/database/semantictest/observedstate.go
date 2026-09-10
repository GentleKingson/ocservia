package semantictest

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/observedstate"
	"github.com/google/uuid"
)

func ObservedStateTransactions(t *testing.T, b database.Backend, node uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	text := "member"
	blank := ""
	group := observedstate.Group{Name: "staff", Members: value.TextArray{Valid: true, Dimensions: []value.Dimension{{Length: 2, LowerBound: -2}, {Length: 2, LowerBound: 5}}, Elements: []*string{&text, nil, &blank, &text}}, Revision: 3, Fingerprint: make([]byte, 32), ObservedAt: value.Timestamp{Valid: true, Micros: value.EndTimestamp - 1}}
	detail, err := value.ParseJSONB([]byte(`{"decimal":1e1000,"tiny":1e-1000,"exact":9007199254740993,"last":1,"last":2}`))
	if err != nil {
		t.Fatal(err)
	}
	event := observedstate.SecurityEvent{ID: uuid.New(), ObservedAt: value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, Severity: "info", Type: "domain-parity", Detail: detail}
	write := func(after error) error {
		return database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
			if err := observedstate.ReplaceGroups(ctx, tx, node, []observedstate.Group{group}); err != nil {
				return err
			}
			if err := observedstate.InsertSecurity(ctx, tx, node, event); err != nil {
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
			s, err := observedstate.FromTransaction(tx)
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
		return observedstate.InsertSecurity(ctx, tx, uuid.New(), observedstate.SecurityEvent{ID: uuid.New(), ObservedAt: event.ObservedAt, Severity: event.Severity, Type: event.Type, Detail: event.Detail})
	}); !errors.Is(err, database.ErrForeignKey) {
		t.Fatal("foreign key not enforced", err)
	}
}
