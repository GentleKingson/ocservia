package approvaltest

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

// Workflow exercises the production service with real runtime credentials.
func Workflow(t *testing.T, b database.Backend, workspace, requester, approver, session, approverSession uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	s := approvals.NewBackend(b)
	hash, summary := approvals.GenericBinding("service.reload", "workspace", workspace)
	request := approvals.Request{WorkspaceID: workspace, RequesterID: requester, ResourceID: workspace, Action: "service.reload", ResourceType: "workspace", Reason: "planned maintenance", TTL: time.Hour, SessionID: session, RequestID: uuid.NewString(), RequestHash: hash, RequestSummary: summary, AuthorityResources: []approvals.AuthorityResource{{WorkspaceID: workspace, Type: "workspace"}, {WorkspaceID: workspace, Type: "workspace"}}}
	pending, err := s.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := s.AuthorityResources(ctx, pending.ID)
	if err != nil || len(scopes) != 1 || scopes[0].ID != uuid.Nil {
		t.Fatalf("authority snapshot: %+v %v", scopes, err)
	}
	if _, err = s.Approve(ctx, approvals.Decision{ApprovalID: pending.ID, ApproverID: requester, SessionID: session, Reason: "self", RequestID: uuid.NewString()}); !errors.Is(err, approvals.ErrSelf) {
		t.Fatalf("self approval: %v", err)
	}
	decision := approvals.Decision{ApprovalID: pending.ID, ApproverID: approver, SessionID: approverSession, Reason: "reviewed", RequestID: uuid.NewString(), ExpectedRequestHash: "wrong"}
	if _, err = s.Approve(ctx, decision); !errors.Is(err, approvals.ErrNotReady) {
		t.Fatalf("hash substitution: %v", err)
	}
	decision.ExpectedRequestHash = pending.RequestHash
	approved, err := s.Approve(ctx, decision)
	if err != nil || approved.Status != "approved" {
		t.Fatalf("approve: %+v %v", approved, err)
	}
	if err = s.ValidateApprovedBound(ctx, pending.ID, workspace, requester, request.Action, request.ResourceType, workspace, hash); err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("cancel business mutation")
	err = database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		if err := approvals.ConsumeBoundTx(ctx, tx, pending.ID, workspace, requester, request.Action, request.ResourceType, workspace, hash); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if err = s.ValidateApprovedBound(ctx, pending.ID, workspace, requester, request.Action, request.ResourceType, workspace, hash); err != nil {
		t.Fatalf("consume rollback: %v", err)
	}
	err = database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		if err := approvals.ConsumeBoundTx(ctx, tx, pending.ID, workspace, requester, request.Action, request.ResourceType, workspace, hash); err != nil {
			return err
		}
		return approvals.ValidateConsumedBoundTx(ctx, tx, pending.ID, workspace, requester, request.Action, request.ResourceType, workspace, hash)
	})
	if err != nil {
		t.Fatal(err)
	}
	err = database.Within(ctx, b, database.ReadCommitted, func(tx database.Tx) error {
		return approvals.ConsumeBoundTx(ctx, tx, pending.ID, workspace, requester, request.Action, request.ResourceType, workspace, hash)
	})
	if !errors.Is(err, approvals.ErrNotReady) {
		t.Fatalf("replay: %v", err)
	}
	stored, err := s.Get(ctx, pending.ID)
	if err != nil || stored.Status != "consumed" || stored.RequestHash != pending.RequestHash {
		t.Fatalf("stored approval: %+v %v", stored, err)
	}
	// PostgreSQL JSONB accepts decimal exponents outside MySQL native JSON's
	// double range. Exercise the real Create/Get path, including duplicate keys.
	request.RequestSummary = []byte("\t {\"number\":1e1000,\"duplicate\":0,\"duplicate\":1.2300}")
	digest := sha256.Sum256(request.RequestSummary)
	request.RequestHash = digest[:]
	request.RequestID = uuid.NewString()
	large, err := s.Create(ctx, request)
	if err != nil {
		t.Fatalf("large JSON create: %v", err)
	}
	large, err = s.Get(ctx, large.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, err := value.ParseJSONB(request.RequestSummary)
	if err != nil {
		t.Fatal(err)
	}
	got, err := value.ParseJSONB(large.RequestSummary)
	if err != nil || string(got.Bytes()) != string(want.Bytes()) {
		t.Fatalf("lossy approval JSON: %v", err)
	}
	request.RequestSummary = []byte(`{"nul":"\u0000"}`)
	request.RequestID = uuid.NewString()
	if _, err = s.Create(ctx, request); err == nil {
		t.Fatal("approval JSON accepted NUL")
	}
}

// ExtendedTimes mutates clocks through the owner fixture, then exercises only
// runtime-principal read/approve paths. Other approval bindings stay unchanged.
func ExtendedTimes(t *testing.T, b database.Backend, workspace, requester, approver, session, approverSession uuid.UUID, set func(uuid.UUID, value.Timestamp, value.Timestamp)) {
	t.Helper()
	ctx := context.Background()
	s := approvals.NewBackend(b)
	hash, summary := approvals.GenericBinding("service.reload", "workspace", workspace)
	request := approvals.Request{WorkspaceID: workspace, RequesterID: requester, ResourceID: workspace, Action: "service.reload", ResourceType: "workspace", Reason: "extended clocks", TTL: time.Hour, SessionID: session, RequestID: uuid.NewString(), RequestHash: hash, RequestSummary: summary, AuthorityResources: []approvals.AuthorityResource{{WorkspaceID: workspace, Type: "workspace"}}}
	negative, positive := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	// expires_at must be strictly greater than created_at, so negative
	// infinity is valid for creation but cannot be a valid stored expiry.
	ancient := value.Timestamp{Valid: true, Micros: value.MinTimestamp}
	for _, expires := range []value.Timestamp{ancient, positive} {
		pending, err := s.Create(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		set(pending.ID, negative, expires)
		read, err := s.Get(ctx, pending.ID)
		if err != nil || read.CreatedAt != negative || read.ExpiresAt != expires {
			t.Fatal("extended approval read", read, err)
		}
		approved, err := s.Approve(ctx, approvals.Decision{ApprovalID: pending.ID, ApproverID: approver, SessionID: approverSession, Reason: "reviewed", RequestID: uuid.NewString(), ExpectedRequestHash: pending.RequestHash})
		if expires == ancient {
			if !errors.Is(err, approvals.ErrNotReady) {
				t.Fatal("expired extended approval", err)
			}
		} else if err != nil || approved.Status != "approved" || approved.ExpiresAt != positive {
			t.Fatal("infinite approval", approved, err)
		}
	}
}
