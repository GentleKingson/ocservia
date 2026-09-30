// Package historyretention owns bounded history compaction, independently of rebind.
package historyretention

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	retentionstore "github.com/GentleKingson/ocservia/control-plane/internal/historyretention/store"
	"google.golang.org/protobuf/proto"
)

const BatchSize = 32

type Policy struct{ RetiredNodeDays, CommandDays int }

func DefaultPolicy() Policy { return Policy{90, 90} }
func (p Policy) Validate() error {
	if p.RetiredNodeDays < 30 || p.RetiredNodeDays > 730 || p.CommandDays < 30 || p.CommandDays > 365 {
		return errors.New("invalid history retention days: retired node 30–730, command 30–365; zero does not disable retention")
	}
	return nil
}

type Command = retentionstore.Command
type Provider = retentionstore.Provider

type Service struct {
	backend database.Backend
	policy  Policy
	now     func() time.Time
}

func New(backend database.Backend, policy Policy) (*Service, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &Service{backend: backend, policy: policy, now: time.Now}, nil
}

// RunOnce commits at most one bounded batch. The next scheduler tick resumes
// from unmarked rows. No command identity, replay fence or recovery proof is deleted.
func (s *Service) RunOnce(ctx context.Context) error {
	now := s.now().UTC()
	at, err := value.FromTime(now)
	if err != nil {
		return err
	}
	cutoff, err := value.FromTime(now.Add(-time.Duration(s.policy.CommandDays) * 24 * time.Hour))
	if err != nil {
		return err
	}
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		provider, ok := tx.(Provider)
		if !ok {
			return database.ErrUnsupported
		}
		store := provider.HistoryRetentionStore()
		ids, err := store.Candidates(ctx, cutoff)
		if err != nil {
			return err
		}
		for _, id := range ids {
			command, err := store.LockCommand(ctx, id, cutoff)
			if errors.Is(err, database.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			compact, err := compactEnvelope(command, now)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(command.Envelope)
			if err := store.Compact(ctx, command, compact, digest[:], at); err != nil {
				return err
			}
		}
		return coordination.AssertFenceTx(ctx, tx, coordination.FenceFromContext(ctx))
	})
	if err != nil {
		return err
	}
	retiredCutoff, err := value.FromTime(now.Add(-time.Duration(s.policy.RetiredNodeDays) * 24 * time.Hour))
	if err != nil {
		return err
	}
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		provider, ok := tx.(Provider)
		if !ok {
			return database.ErrUnsupported
		}
		if err := provider.HistoryRetentionStore().CompactRetired(ctx, retiredCutoff); err != nil {
			return err
		}
		return coordination.AssertFenceTx(ctx, tx, coordination.FenceFromContext(ctx))
	})

}
func compactEnvelope(command Command, now time.Time) ([]byte, error) {
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(command.Envelope, &envelope); err != nil {
		return nil, err
	}
	if !bytes.Equal(envelope.CommandId, command.ID[:]) || !bytes.Equal(envelope.NodeId, command.NodeID[:]) || envelope.ExpiresAt == nil || envelope.ExpiresAt.CheckValid() != nil || !envelope.ExpiresAt.AsTime().Before(now) {
		return nil, errors.New("command retention: invalid identity or live signed replay boundary")
	}
	// Preserve signed authorization, semantic hash, revisions and connection fence.
	// This header is evidence only and is never dispatched as a command.
	if len(envelope.ProtoReflect().GetUnknown()) != 0 {
		return nil, errors.New("command retention: unknown envelope fields require a compatible reader")
	}
	envelope.Payload = nil
	return proto.Marshal(&envelope)
}
