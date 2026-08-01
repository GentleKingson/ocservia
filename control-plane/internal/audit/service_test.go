package audit

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type fakeTx struct{ committed, rolledBack bool }

func (t *fakeTx) Commit(context.Context) error   { t.committed = true; return nil }
func (t *fakeTx) Rollback(context.Context) error { t.rolledBack = true; return nil }

type fakeTxManager struct{ tx *fakeTx }

func (m fakeTxManager) Begin(context.Context) (Tx, error) { return m.tx, nil }

type fakeIntents struct {
	appended bool
	err      error
}

func (r *fakeIntents) AppendIntent(context.Context, Tx, Intent) error {
	r.appended = true
	return r.err
}

func TestWithIntentCommitsAuditAndChangeTogether(t *testing.T) {
	tx := &fakeTx{}
	repository := &fakeIntents{}
	service := NewService(fakeTxManager{tx: tx}, repository)
	changed := false
	err := service.WithIntent(context.Background(), validIntent(), func(context.Context, Tx) error { changed = true; return nil })
	if err != nil {
		t.Fatalf("WithIntent() error = %v", err)
	}
	if !repository.appended || !changed || !tx.committed {
		t.Fatal("audit intent, change, and commit did not complete")
	}
}

func TestWithIntentRejectsChangeWhenAuditFails(t *testing.T) {
	tx := &fakeTx{}
	repository := &fakeIntents{err: errors.New("audit unavailable")}
	service := NewService(fakeTxManager{tx: tx}, repository)
	changed := false
	err := service.WithIntent(context.Background(), validIntent(), func(context.Context, Tx) error { changed = true; return nil })
	if err == nil {
		t.Fatal("WithIntent() succeeded after audit failure")
	}
	if changed || tx.committed {
		t.Fatal("business change committed after audit failure")
	}
}

func validIntent() Intent {
	return Intent{ID: uuid.MustParse("0193c1b2-7d8a-7f01-8a2d-1e5b4f302901"), WorkspaceID: uuid.MustParse("0193c1b2-7d8a-7f01-8a2d-1e5b4f302902"), Action: "test.write", RequestID: "request-1"}
}
