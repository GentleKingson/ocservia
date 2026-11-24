package telemetry

import (
	"context"

	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
)

func replaceObservedGroups(ctx context.Context, store telemetrystore.Store, batch Batch) error {
	stamp := batch.Snapshot.ObservedAt.UTC()
	groups := make([]telemetrystore.Group, 0, len(batch.Groups))
	for _, g := range batch.Groups {
		members := g.Members
		if members == nil {
			members = []string{}
		}
		groups = append(groups, telemetrystore.Group{Name: g.Name, Members: members, Revision: int64(g.Revision), ObservedAt: stamp})
	}
	return store.ReplaceGroups(ctx, batch.NodeID, groups)
}
