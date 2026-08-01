package audit

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

type Intent struct {
	ID           uuid.UUID
	WorkspaceID  uuid.UUID
	OccurredAt   time.Time
	ActorType    string
	ActorID      string
	Action       string
	ResourceType string
	ResourceID   *uuid.UUID
	RequestID    string
	TraceID      string
	Reason       string
}

type Tx interface {
	Commit(context.Context) error
	Rollback(context.Context) error
}
type TxManager interface {
	Begin(context.Context) (Tx, error)
}
type IntentRepository interface {
	AppendIntent(context.Context, Tx, Intent) error
}

type Service struct {
	transactions TxManager
	intents      IntentRepository
}

func NewService(transactions TxManager, intents IntentRepository) *Service {
	return &Service{transactions: transactions, intents: intents}
}

func (s *Service) WithIntent(ctx context.Context, intent Intent, change func(context.Context, Tx) error) error {
	if intent.ID == uuid.Nil || intent.WorkspaceID == uuid.Nil || intent.Action == "" || intent.RequestID == "" {
		return errors.New("invalid audit intent")
	}
	tx, err := s.transactions.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if err := s.intents.AppendIntent(ctx, tx, intent); err != nil {
		return err
	}
	if err := change(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
