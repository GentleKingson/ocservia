package telemetry

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/observedstate"
)

func replaceObservedGroups(ctx context.Context, tx database.Tx, batch Batch) error {
	stamp, err := value.FromTime(batch.Snapshot.ObservedAt)
	if err != nil {
		return err
	}
	groups := make([]observedstate.Group, 0, len(batch.Groups))
	for _, g := range batch.Groups {
		groups = append(groups, observedstate.Group{Name: g.Name, Members: value.TextList(g.Members), Revision: int64(g.Revision), Fingerprint: g.Fingerprint, ObservedAt: stamp})
	}
	return observedstate.ReplaceGroups(ctx, tx, batch.NodeID, groups)
}

func insertObservedSecurity(ctx context.Context, tx database.Tx, batch Batch) error {
	for _, e := range batch.Security {
		stamp, err := value.FromTime(e.ObservedAt)
		if err != nil {
			return err
		}
		detail, err := value.ParseJSONB(e.Detail)
		if err != nil {
			return err
		}
		if err = observedstate.InsertSecurity(ctx, tx, batch.NodeID, observedstate.SecurityEvent{ID: e.ID, ObservedAt: stamp, Severity: e.Severity, Type: e.Type, Detail: detail}); err != nil {
			return err
		}
	}
	return nil
}
