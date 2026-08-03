package enrollment

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAuditPayloadIsUnambiguous(t *testing.T) {
	base := auditRecord{WorkspaceID: uuid.Must(uuid.NewV7()), At: time.Now().UTC(), ActorType: "user", ActorID: "operator", Action: "node.approve", ResourceType: "node", ResourceID: uuid.Must(uuid.NewV7())}
	first := base
	first.RequestID, first.Reason = "a|b", "c"
	second := base
	second.RequestID, second.Reason = "a", "b|c"
	eventID := uuid.Must(uuid.NewV7())
	firstPayload, err := encodeAuditPayload([]byte("previous"), eventID, first)
	if err != nil {
		t.Fatal(err)
	}
	secondPayload, err := encodeAuditPayload([]byte("previous"), eventID, second)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstPayload, secondPayload) {
		t.Fatal("distinct audit fields produced the same payload")
	}
	otherIDPayload, err := encodeAuditPayload([]byte("previous"), uuid.Must(uuid.NewV7()), first)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstPayload, otherIDPayload) {
		t.Fatal("distinct audit event IDs produced the same payload")
	}
}

func TestValidShortRejectsWhitespaceOutsideContractLimit(t *testing.T) {
	if validShort("value ", 5) {
		t.Fatal("expected trailing whitespace to count toward the contract limit")
	}
	if validShort(" "+strings.Repeat("x", 5), 5) {
		t.Fatal("expected leading whitespace to be rejected")
	}
	if !validShort("value", 5) {
		t.Fatal("expected an exact-length value to be accepted")
	}
	if !validShort(strings.Repeat("界", 5), 5) {
		t.Fatal("expected the contract limit to count Unicode characters")
	}
	if validShort(strings.Repeat("界", 6), 5) {
		t.Fatal("expected an over-limit Unicode value to be rejected")
	}
}

func TestValidateLockedTokenPreservesQueryErrors(t *testing.T) {
	queryErr := errors.New("database unavailable")
	err := validateLockedToken(queryErr, false, time.Time{}, time.Now())
	if !errors.Is(err, queryErr) || errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected query error, got %v", err)
	}
}

func TestConsumedTokenValidationIgnoresExpiry(t *testing.T) {
	if err := validateLockedToken(nil, true, time.Now().Add(-time.Minute), time.Now()); err != nil {
		t.Fatalf("consumed token expiry should be checked by the idempotency path: %v", err)
	}
	if err := validateLockedToken(nil, false, time.Now().Add(-time.Minute), time.Now()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unconsumed expired token error = %v", err)
	}
}

func TestStoredValuesRejectSurroundingWhitespace(t *testing.T) {
	service := &Service{}
	nodeID := uuid.Must(uuid.NewV7())
	approval := Approval{NodeID: nodeID, Labels: map[string]string{"region": "test "}, Policy: "readonly", Capabilities: []string{"ocserv.status.read"}, ActorID: "operator", Reason: "approve", RequestID: uuid.Must(uuid.NewV7()).String()}
	if _, err := service.Approve(context.Background(), approval); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected an invalid label value, got %v", err)
	}
	revocation := Revocation{NodeID: nodeID, ActorID: "operator", Reason: "revoke ", RequestID: uuid.Must(uuid.NewV7()).String()}
	if _, err := service.Revoke(context.Background(), revocation); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected an invalid revocation reason, got %v", err)
	}
}
