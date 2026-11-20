package telemetry

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
)

var (
	observedGroupName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	errInvalidState   = errors.New("invalid observed state")
)

func replaceObservedGroups(ctx context.Context, tx database.Tx, store telemetrystore.Store, batch Batch) error {
	stamp, err := value.FromTime(batch.Snapshot.ObservedAt)
	if err != nil {
		return err
	}
	groups := make([]telemetrystore.Group, 0, len(batch.Groups))
	seen := make(map[string]bool, len(batch.Groups))
	for _, g := range batch.Groups {
		members := value.TextList(g.Members)
		if !observedGroupName.MatchString(g.Name) || seen[g.Name] || !members.Valid || members.Validate() != nil || len(members.Elements) > 4096 || len(g.Fingerprint) != 32 || !stamp.Valid || stamp.Validate() != nil {
			return errInvalidState
		}
		seen[g.Name] = true
		groups = append(groups, telemetrystore.Group{Name: g.Name, Members: members, Revision: int64(g.Revision), Fingerprint: g.Fingerprint, ObservedAt: stamp})
	}
	return store.ReplaceGroups(ctx, batch.NodeID, groups)
}

func insertObservedSecurity(ctx context.Context, store telemetrystore.Store, batch Batch) error {
	events := make([]telemetrystore.SecurityEvent, 0, len(batch.Security))
	for _, e := range batch.Security {
		stamp, err := value.FromTime(e.ObservedAt)
		if err != nil {
			return err
		}
		detail, err := value.ParseJSONB(e.Detail)
		if err != nil {
			return err
		}
		if !stamp.Valid || stamp.Validate() != nil || !utf8.ValidString(e.Type) || strings.IndexByte(e.Type, 0) >= 0 || utf8.RuneCountInString(e.Type) < 1 || utf8.RuneCountInString(e.Type) > 128 || e.Severity != "info" && e.Severity != "warning" && e.Severity != "critical" || !detail.Valid() || len(detail.Bytes()) == 0 || detail.Bytes()[0] != '{' {
			return errInvalidState
		}
		events = append(events, telemetrystore.SecurityEvent{ID: e.ID, ObservedAt: stamp, Severity: e.Severity, Type: e.Type, Detail: detail})
	}
	return store.InsertSecurity(ctx, batch.NodeID, events)
}
