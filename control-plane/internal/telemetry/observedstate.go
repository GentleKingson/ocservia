package telemetry

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
)

var (
	observedGroupName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	errInvalidState   = errors.New("invalid observed state")
)

func replaceObservedGroups(ctx context.Context, _ database.Tx, store telemetrystore.Store, batch Batch) error {
	if batch.Snapshot.ObservedAt.IsZero() {
		return errInvalidState
	}
	stamp := batch.Snapshot.ObservedAt.UTC()
	groups := make([]telemetrystore.Group, 0, len(batch.Groups))
	seen := make(map[string]bool, len(batch.Groups))
	for _, g := range batch.Groups {
		if !observedGroupName.MatchString(g.Name) || seen[g.Name] || len(g.Members) > 4096 || len(g.Fingerprint) != 32 {
			return errInvalidState
		}
		for _, member := range g.Members {
			if !utf8.ValidString(member) || strings.IndexByte(member, 0) >= 0 {
				return errInvalidState
			}
		}
		seen[g.Name] = true
		members := g.Members
		if members == nil {
			members = []string{}
		}
		groups = append(groups, telemetrystore.Group{Name: g.Name, Members: members, Revision: int64(g.Revision), Fingerprint: g.Fingerprint, ObservedAt: stamp})
	}
	return store.ReplaceGroups(ctx, batch.NodeID, groups)
}

func insertObservedSecurity(ctx context.Context, store telemetrystore.Store, batch Batch) error {
	events := make([]telemetrystore.SecurityEvent, 0, len(batch.Security))
	for _, e := range batch.Security {
		if e.ObservedAt.IsZero() || !utf8.ValidString(e.Type) || strings.IndexByte(e.Type, 0) >= 0 || utf8.RuneCountInString(e.Type) < 1 || utf8.RuneCountInString(e.Type) > 128 || (e.Severity != "info" && e.Severity != "warning" && e.Severity != "critical") || !validObject(e.Detail) {
			return errInvalidState
		}
		events = append(events, telemetrystore.SecurityEvent{ID: e.ID, ObservedAt: e.ObservedAt.UTC(), Severity: e.Severity, Type: e.Type, Detail: e.Detail})
	}
	return store.InsertSecurity(ctx, batch.NodeID, events)
}
