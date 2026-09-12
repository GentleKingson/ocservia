package enrollment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation/attestationstore"
	"github.com/google/uuid"
)

var errAttestationRollback = errors.New("PR-05 injected registration rollback")

type attestationRollbackBackend struct{ database.Backend }
type attestationRollbackTx struct{ database.Tx }
type attestationRollbackStore struct{ attestationstore.Store }

func (b attestationRollbackBackend) Begin(ctx context.Context, isolation database.Isolation) (database.Tx, error) {
	tx, err := b.Backend.Begin(ctx, isolation)
	if err != nil {
		return nil, err
	}
	return attestationRollbackTx{tx}, nil
}
func (tx attestationRollbackTx) PrivdAttestationStore() attestationstore.Store {
	s, _ := attestationstore.From(tx.Tx)
	return attestationRollbackStore{s}
}
func (s attestationRollbackStore) BumpNodeRevision(ctx context.Context, node uuid.UUID, at time.Time) error {
	if err := s.Store.BumpNodeRevision(ctx, node, at); err != nil {
		return err
	}
	return errAttestationRollback
}

func TestAttestationSafetyBackendIntegration(t *testing.T) {
	f, ctx := newLifecycleFixture(t), context.Background()
	identity, session := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	at, _ := value.FromTime(time.Now().UTC())
	expires, _ := at.Add(time.Hour)
	f.exec(t, `INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES($1,'pr05',$2,$3,$4)`, `INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES(?,'pr05',?,?,?)`, identity, identity.String(), at, at)
	f.exec(t, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES($1,$2,$3,$4)`, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES(?,?,?,?)`, session, identity, expires, at)
	service := privdattestation.NewBackend(f.b)
	newNode := func() uuid.UUID {
		t.Helper()
		node := uuid.Must(uuid.NewV7())
		f.within(t, func(tx database.Tx) error {
			s, err := enrollmentstore.Enrollment(tx)
			if err != nil {
				return err
			}
			if err := s.InsertNode(ctx, node, f.workspace, node.String(), at); err != nil {
				return err
			}
			if err := s.InsertEndpoint(ctx, node, append(bytes.Clone(node[:]), node[:]...), at); err != nil {
				return err
			}
			_, err = s.Activate(ctx, node, "{}", "standard", at)
			return err
		})
		return node
	}
	credential := func(node uuid.UUID) privdattestation.Credential {
		t.Helper()
		c, err := service.CreateCredential(ctx, privdattestation.CredentialRequest{NodeID: node, IdentityID: identity, SessionID: session, TTL: 15 * time.Minute, RequestID: uuid.NewString(), Reason: "PR-05"})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	registration := func(node uuid.UUID, c privdattestation.Credential, key ed25519.PrivateKey) privdattestation.RegistrationRequest {
		t.Helper()
		public := key.Public().(ed25519.PublicKey)
		r := &agentv1.PrivdAttestationRegistrationV1{Version: agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1, NodeId: node[:], PrivdAttestationKeyId: privdattestation.PublicKeyID(public), PublicKey: public, ControllerNonce: c.ControllerNonce, CredentialContextSha256: c.CredentialContextSHA256}
		canonical, err := privdattestation.CanonicalRegistrationV1(r)
		if err != nil {
			t.Fatal(err)
		}
		r.Signature = ed25519.Sign(key, canonical)
		return privdattestation.RegistrationRequest{NodeID: node, Credential: c.Value, Registration: r, RequestID: uuid.NewString()}
	}
	newKey := func() ed25519.PrivateKey {
		t.Helper()
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	keyRecord := func(node uuid.UUID, key string) attestationstore.VerificationKey {
		t.Helper()
		var record attestationstore.VerificationKey
		f.within(t, func(tx database.Tx) error {
			s, err := attestationstore.From(tx)
			if err != nil {
				return err
			}
			record, err = s.VerificationKey(ctx, node, key)
			return err
		})
		return record
	}
	node, key := newNode(), newKey()
	c := credential(node)
	request := registration(node, c, key)
	t.Run("concurrent-consumption", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make(chan error, 8)
		for range 8 {
			wg.Add(1)
			go func() { defer wg.Done(); _, err := service.Register(ctx, request); results <- err }()
		}
		wg.Wait()
		success := 0
		for range 8 {
			err := <-results
			if err == nil {
				success++
			} else if !errors.Is(err, privdattestation.ErrCredential) {
				t.Fatal(err)
			}
		}
		if success != 1 {
			t.Fatal("one-time root credential", success)
		}
		if got := keyRecord(node, request.Registration.PrivdAttestationKeyId); !bytes.Equal(got.PublicKey, key.Public().(ed25519.PublicKey)) {
			t.Fatal("public key changed")
		}
	})
	t.Run("rotation-rollback-and-duplicate-key", func(t *testing.T) {
		rotation := credential(node)
		next := registration(node, rotation, newKey())
		failed := privdattestation.NewBackend(attestationRollbackBackend{f.b})
		if _, err := failed.Register(ctx, next); !errors.Is(err, errAttestationRollback) {
			t.Fatal(err)
		}
		if got := keyRecord(node, request.Registration.PrivdAttestationKeyId); got.ValidUntil != nil {
			t.Fatal("rollback bounded predecessor")
		}
		if _, err := service.Register(ctx, next); err != nil {
			t.Fatal("rollback consumed credential", err)
		}
		if got := keyRecord(node, request.Registration.PrivdAttestationKeyId); got.ValidUntil == nil {
			t.Fatal("rotation did not bound predecessor")
		}
		other := newNode()
		otherCredential := credential(other)
		duplicate := registration(other, otherCredential, key)
		if _, err := service.Register(ctx, duplicate); !errors.Is(err, database.ErrUnique) {
			t.Fatal("duplicate global key accepted", err)
		}
		if _, err := service.Register(ctx, registration(other, otherCredential, newKey())); err != nil {
			t.Fatal("duplicate conflict consumed credential", err)
		}
	})
	t.Run("node-binding-and-revocation", func(t *testing.T) {
		other := newNode()
		c := credential(other)
		if _, err := service.Register(ctx, registration(node, c, newKey())); !errors.Is(err, privdattestation.ErrCredential) {
			t.Fatal("credential rebound", err)
		}
		f.within(t, func(tx database.Tx) error {
			s, err := enrollmentstore.Enrollment(tx)
			if err != nil {
				return err
			}
			at, err := database.WallTime(ctx, tx)
			if err != nil {
				return err
			}
			_, err = s.Revoke(ctx, other, at)
			return err
		})
		if _, err := service.Register(ctx, registration(other, c, newKey())); !errors.Is(err, privdattestation.ErrCredential) {
			t.Fatal("revoked node registered", err)
		}
		if err := service.Revoke(ctx, privdattestation.RevokeRequest{NodeID: node, IdentityID: identity, SessionID: session, KeyID: request.Registration.PrivdAttestationKeyId, RequestID: uuid.NewString(), Reason: "retire"}); err != nil {
			t.Fatal(err)
		}
		if got := keyRecord(node, request.Registration.PrivdAttestationKeyId); got.State != "revoked" || got.ValidUntil == nil {
			t.Fatal("key revocation", got)
		}
	})
	t.Run("expiry-during-node-lock-wait", func(t *testing.T) {
		other := newNode()
		c := credential(other)
		r := registration(other, c, newKey())
		deadline, _ := value.FromTime(time.Now().Add(500 * time.Millisecond))
		f.exec(t, `UPDATE privd_attestation_enrollment_credentials SET expires_at=$1 WHERE id=$2`, `UPDATE privd_attestation_enrollment_credentials SET expires_at=? WHERE id=?`, deadline, c.ID)
		lock, err := f.b.Begin(ctx, database.ReadCommitted)
		if err != nil {
			t.Fatal(err)
		}
		defer rollback(lock)
		s, err := attestationstore.From(lock)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.LockActiveNode(ctx, other); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := service.Register(ctx, r); done <- err }()
		select {
		case err := <-done:
			t.Fatal("registration escaped node lock", err)
		case <-time.After(time.Second):
		}
		if err := lock.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, privdattestation.ErrCredential) {
			t.Fatal("credential expired while waiting", err)
		}
	})
	if result, err := audit.NewBackendManager(f.b, nil).Verify(ctx, f.workspace); err != nil || !result.Valid {
		t.Fatal("attestation audit chain", result, err)
	}
}
