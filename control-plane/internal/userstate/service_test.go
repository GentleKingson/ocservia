package userstate

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testTraceparent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"

func TestMutationValidationAndPasswordFingerprintRedaction(t *testing.T) {
	base := MutationRequest{NodeID: uuid.Must(uuid.NewV7()), Kind: UserCreate, Name: "alice", SealedPassword: bytes.Repeat([]byte{0xa5}, 64), SecretKeyID: "node-key-1", IdempotencyKey: "key", ExpectedVersion: 0, TTL: time.Hour, ActorID: "operator", Reason: "ticket", RequestID: "request", Traceparent: testTraceparent}
	if err := validateMutation(base); err != nil {
		t.Fatalf("valid create: %v", err)
	}
	for _, name := range []string{"", "../alice", "alice bob", "alice;id", string(bytes.Repeat([]byte{'a'}, 65))} {
		request := base
		request.Name = name
		if err := validateMutation(request); err == nil {
			t.Fatalf("accepted unsafe name %q", name)
		}
	}
	withoutSecret := base
	withoutSecret.SealedPassword = nil
	if err := validateMutation(withoutSecret); err == nil {
		t.Fatal("accepted missing sealed password")
	}
	first := desiredFingerprint(UserCreate, "alice", nil)
	second := desiredFingerprint(UserPasswordRotate, "alice", nil)
	if first != second {
		t.Fatal("password material affected public desired fingerprint")
	}
}

func TestMembersAreCanonicalAndRequestHashBindsCiphertext(t *testing.T) {
	request := MutationRequest{NodeID: uuid.Must(uuid.NewV7()), Kind: GroupApply, Name: "staff", Members: []string{"bob", "alice", "bob"}, IdempotencyKey: "key", ExpectedVersion: 0, TTL: time.Hour, ActorID: "operator", Reason: "ticket", RequestID: "request", Traceparent: testTraceparent}
	if got := normalizeMembers(request.Members); len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("members=%v", got)
	}
	secret := request
	secret.Kind = UserCreate
	secret.Name = "alice"
	secret.Members = nil
	secret.SecretKeyID = "key"
	secret.SealedPassword = bytes.Repeat([]byte{1}, 32)
	changed := secret
	changed.SealedPassword = bytes.Repeat([]byte{2}, 32)
	if requestHash(secret) == requestHash(changed) {
		t.Fatal("idempotency hash did not bind sealed ciphertext")
	}
}

func TestConvergenceRequiresAppliedRevisionAndPendingWins(t *testing.T) {
	desired, observed := int64(2), int64(1)
	state := ResourceState{
		DesiredVersion:      &desired,
		DesiredRevision:     &desired,
		ObservedRevision:    &observed,
		DesiredFingerprint:  "same",
		ObservedFingerprint: "same",
	}
	if got := convergence(state, "active"); got != "drifted" {
		t.Fatalf("stale applied revision reported %q", got)
	}
	pending := "queued"
	state.OperationState = &pending
	if got := convergence(state, "offline"); got != "offline_pending" {
		t.Fatalf("offline queued revision reported %q", got)
	}
	state.OperationState = nil
	state.ObservedRevision = &desired
	if got := convergence(state, "active"); got != "converged" {
		t.Fatalf("matching revision and fingerprint reported %q", got)
	}
}
