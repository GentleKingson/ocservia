package useroperations

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/google/uuid"
)

const testTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestPolicyValidationDefinesBytesPeriodsAndUTC(t *testing.T) {
	nodeID := uuid.Must(uuid.NewV7())
	base := PolicyRequest{NodeID: nodeID, Username: "alice", QuotaPeriod: "monthly", QuotaDirection: "rxtx", QuotaBytes: 1024, IdempotencyKey: "policy-1", ActorID: "operator", Reason: "ticket", RequestID: "request-1", Traceparent: testTraceparent}
	utc := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	base.ExpiresAt = &utc
	if err := validatePolicyRequest(base); err != nil {
		t.Fatalf("valid policy: %v", err)
	}

	tests := []struct {
		name   string
		change func(*PolicyRequest)
	}{
		{"free quota must use none", func(value *PolicyRequest) { value.QuotaPeriod = "none" }},
		{"none quota must be zero", func(value *PolicyRequest) { value.QuotaPeriod, value.QuotaBytes = "none", 1 }},
		{"unknown direction", func(value *PolicyRequest) { value.QuotaDirection = "both-ish" }},
		{"unsafe JSON integer quota", func(value *PolicyRequest) { value.QuotaBytes = MaxSafeQuotaBytes + 1 }},
		{"invalid username", func(value *PolicyRequest) { value.Username = "alice/../../root" }},
		{"offset expiry", func(value *PolicyRequest) {
			expiry := time.Date(2026, 9, 1, 0, 0, 0, 0, time.FixedZone("offset", 3600))
			value.ExpiresAt = &expiry
		}},
		{"fractional expiry", func(value *PolicyRequest) { expiry := utc.Add(time.Nanosecond); value.ExpiresAt = &expiry }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			test.change(&value)
			if !errors.Is(validatePolicyRequest(value), ErrInvalidRequest) {
				t.Fatal("invalid policy accepted")
			}
		})
	}
}

func TestQuotaDirectionAndMonthlyBoundary(t *testing.T) {
	if got := quotaValue("rx", 10, 20); got != 10 {
		t.Fatalf("rx=%d", got)
	}
	if got := quotaValue("tx", 10, 20); got != 20 {
		t.Fatalf("tx=%d", got)
	}
	if got := quotaValue("rxtx", 10, 20); got != 30 {
		t.Fatalf("rxtx=%d", got)
	}
	offset := time.FixedZone("minus-seven", -7*60*60)
	input := time.Date(2026, 9, 30, 20, 0, 0, 0, offset)
	want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if got := monthStart(input); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("month start=%s", got)
	}
}

func TestStableSchedulerIdentityAndBatchLimit(t *testing.T) {
	first := stableKey("policy", "node", "alice", "2", "quota", "2026-08-01T00:00:00Z")
	if second := stableKey("policy", "node", "alice", "2", "quota", "2026-08-01T00:00:00Z"); first != second {
		t.Fatal("scheduler idempotency key changed")
	}
	if len(first) > 128 || !validTraceparent(stableTraceparent(first)) {
		t.Fatalf("invalid stable identities key=%q trace=%q", first, stableTraceparent(first))
	}
	request := BatchRequest{ID: uuid.Must(uuid.NewV7()), WorkspaceID: uuid.Must(uuid.NewV7()), ActorID: "operator", Reason: "ticket", RequestID: "request", Traceparent: testTraceparent, IdempotencyKey: "batch"}
	request.Items = make([]BatchItemRequest, MaxBatchItems+1)
	if !errors.Is(validateBatchRequest(request), ErrInvalidRequest) {
		t.Fatal("oversized batch accepted")
	}
	if DefaultGlobalConcurrency != 50 || !strings.HasPrefix(first, "i14-") {
		t.Fatal("documented global concurrency or key namespace changed")
	}
}

func TestBulkDisableRequiresApproval(t *testing.T) {
	request := BatchRequest{
		ID: uuid.Must(uuid.NewV7()), WorkspaceID: uuid.Must(uuid.NewV7()),
		ActorID: "operator", Reason: "ticket", RequestID: "request",
		Traceparent: testTraceparent, IdempotencyKey: "batch",
		Items: []BatchItemRequest{{NodeID: uuid.Must(uuid.NewV7()), Username: "alice", Action: "disable", ExpectedVersion: 1, Authorized: true}},
	}
	if err := validateBatchRequest(request); !errors.Is(err, approvals.ErrNotReady) {
		t.Fatalf("disable without independent approval=%v", err)
	}
	request.Items[0].Action = "enable"
	if err := validateBatchRequest(request); err != nil {
		t.Fatalf("enable-only batch unexpectedly requires approval: %v", err)
	}
}

func TestBatchApprovalHashBindsOrderedContent(t *testing.T) {
	nodeA, nodeB := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	items := []BatchItemRequest{{NodeID: nodeA, Username: "alice", Action: "disable", ExpectedVersion: 1, Authorized: true}, {NodeID: nodeB, Username: "bob", Action: "enable", ExpectedVersion: 2}}
	original := BatchRequestHash(items)
	reordered := BatchRequestHash([]BatchItemRequest{items[1], items[0]})
	if original == reordered {
		t.Fatal("reordering batch items did not change the approval hash")
	}
	items[0].ExpectedVersion++
	if original == BatchRequestHash(items) {
		t.Fatal("substituting batch content did not change the approval hash")
	}
	items[0].ExpectedVersion--
	items[0].Authorized = false
	if original != BatchRequestHash(items) {
		t.Fatal("server authorization result leaked into the client content hash")
	}
}

func TestBatchValidationRejectsInvalidNamesAndWhitespaceActors(t *testing.T) {
	request := BatchRequest{
		ID: uuid.Must(uuid.NewV7()), WorkspaceID: uuid.Must(uuid.NewV7()),
		ActorID: "operator", Reason: "ticket", RequestID: "request",
		Traceparent: testTraceparent, IdempotencyKey: "batch",
		Items: []BatchItemRequest{{NodeID: uuid.Must(uuid.NewV7()), Username: "../alice", Action: "enable", ExpectedVersion: 1, Authorized: true}},
	}
	if !errors.Is(validateBatchRequest(request), ErrInvalidRequest) {
		t.Fatal("invalid batch username accepted")
	}
	request.Items[0].Username = "alice"
	request.ActorID = "  "
	if !errors.Is(validateBatchRequest(request), ErrInvalidRequest) {
		t.Fatal("whitespace actor accepted")
	}
}
