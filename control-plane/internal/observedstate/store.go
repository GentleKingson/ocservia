// Package observedstate persists the observed groups and security events from
// telemetry using the caller's transaction and lossless logical database values.
package observedstate

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type Group struct {
	Name        string
	Members     value.TextArray
	Revision    int64
	Fingerprint []byte
	ObservedAt  value.Timestamp
}

type SecurityEvent struct {
	ID             uuid.UUID
	ObservedAt     value.Timestamp
	Severity, Type string
	Detail         value.JSONB
}

type Store interface {
	LockNode(context.Context, uuid.UUID) error
	ReplaceGroups(context.Context, uuid.UUID, []Group) error
	Groups(context.Context, uuid.UUID) ([]Group, error)
	InsertSecurity(context.Context, uuid.UUID, SecurityEvent) error
	SecurityEvent(context.Context, uuid.UUID) (SecurityEvent, error)
}

func FromTransaction(tx database.Tx) (Store, error) {
	provider, ok := tx.(interface{ ObservedStateStore() Store })
	if !ok {
		return nil, database.ErrUnsupported
	}
	return provider.ObservedStateStore(), nil
}

var groupName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var ErrInvalid = errors.New("invalid observed state")

func ReplaceGroups(ctx context.Context, tx database.Tx, node uuid.UUID, groups []Group) error {
	seen := make(map[string]bool, len(groups))
	for _, g := range groups {
		if !groupName.MatchString(g.Name) || seen[g.Name] || !g.Members.Valid || g.Members.Validate() != nil || len(g.Members.Elements) > 4096 || g.Revision < 0 || len(g.Fingerprint) != 32 || !g.ObservedAt.Valid || g.ObservedAt.Validate() != nil {
			return ErrInvalid
		}
		seen[g.Name] = true
	}
	s, err := FromTransaction(tx)
	if err != nil {
		return err
	}
	if err = s.LockNode(ctx, node); err != nil {
		return err
	}
	return s.ReplaceGroups(ctx, node, groups)
}

func InsertSecurity(ctx context.Context, tx database.Tx, node uuid.UUID, event SecurityEvent) error {
	if !event.ObservedAt.Valid || event.ObservedAt.Validate() != nil || !utf8.ValidString(event.Type) || strings.IndexByte(event.Type, 0) >= 0 || utf8.RuneCountInString(event.Type) < 1 || utf8.RuneCountInString(event.Type) > 128 || event.Severity != "info" && event.Severity != "warning" && event.Severity != "critical" || !event.Detail.Valid() || len(event.Detail.Bytes()) == 0 || event.Detail.Bytes()[0] != '{' {
		return ErrInvalid
	}
	s, err := FromTransaction(tx)
	if err != nil {
		return err
	}
	return s.InsertSecurity(ctx, node, event)
}
