package mysql

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetrywrite"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

// TestRealPrivdAttestationWorkflow drives the production service over the
// backend-owned stores with real runtime credentials: credential issuance and
// the single-outstanding rule, signed registration with rotation overlap and
// the rotation limit, revocation, exact microsecond storage, and the telemetry
// ingestion key read.
func TestRealPrivdAttestationWorkflow(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	workspace, node, identity, session := uuid.New(), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := owner.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,?,?,?,?)`, UUIDBytes(workspace), "attest", "attest-"+workspace.String(), now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'attest-node','active',?,?)`, UUIDBytes(node), UUIDBytes(workspace), now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO identities(id,issuer,subject,email,display_name,created_at,updated_at) VALUES(?,?,?,'','attest',?,?)`, UUIDBytes(identity), "attest-test", identity.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES(?,?,?,?)`, UUIDBytes(session), UUIDBytes(identity), fixtureTimestamp(t, now.Add(time.Hour)), fixtureTimestamp(t, now)); err != nil {
		t.Fatal(err)
	}
	if err := owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = cfg.FormatDSN()
	runtime, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	service := privdattestation.NewBackend(runtime)

	request := privdattestation.CredentialRequest{NodeID: node, IdentityID: identity, SessionID: session, TTL: 30 * time.Minute, RequestID: uuid.NewString(), Reason: "attest enrollment"}
	credential, err := service.CreateCredential(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.CreateCredential(ctx, request); !errors.Is(err, privdattestation.ErrCredential) {
		t.Fatalf("outstanding credential accepted: %v", err)
	}
	register := func(credential privdattestation.Credential) string {
		t.Helper()
		keyID, err := registerRaw(t, service, node, credential)
		if err != nil {
			t.Fatal(err)
		}
		return keyID
	}
	first := register(credential)
	// Rotation consumes the first credential, so the overlap registration
	// presents a fresh one.
	rotation, err := service.CreateCredential(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	second := register(rotation)
	// Rotation overlap bounds the predecessor exactly 24 hours past the
	// successor's activation instant, reusing one transaction time.
	var firstActivated, secondActivated, predecessorUntil value.Timestamp
	var successor string
	if err = runtime.QueryRow(ctx, `SELECT activated_at FROM node_privd_attestation_keys WHERE key_id=?`, second).Scan(&secondActivated); err != nil {
		t.Fatal(err)
	}
	if err = runtime.QueryRow(ctx, `SELECT activated_at,valid_until,successor_key_id FROM node_privd_attestation_keys WHERE key_id=?`, first).Scan(&firstActivated, &predecessorUntil, &successor); err != nil {
		t.Fatal(err)
	}
	if predecessorUntil.Micros-secondActivated.Micros != int64(24*time.Hour/time.Microsecond) || successor != second {
		t.Fatalf("rotation overlap: %v %v %v %s", firstActivated, secondActivated, predecessorUntil, successor)
	}

	// A third registration exceeds the two-key rotation overlap.
	third, err := service.CreateCredential(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registerRaw(t, service, node, third); !errors.Is(err, privdattestation.ErrRotationLimit) {
		t.Fatalf("rotation limit: %v", err)
	}

	// Credential consumption and the key instant share one transaction time.
	var created, approved, activated value.Timestamp
	var consumed value.Timestamp
	if err = runtime.QueryRow(ctx, `SELECT created_at,approved_at,activated_at FROM node_privd_attestation_keys WHERE key_id=?`, second).Scan(&created, &approved, &activated); err != nil {
		t.Fatal(err)
	}
	if created != approved || approved != activated {
		t.Fatalf("registration instants diverged: %v %v %v", created, approved, activated)
	}
	if err = runtime.QueryRow(ctx, `SELECT consumed_at FROM privd_attestation_enrollment_credentials WHERE id=?`, UUIDBytes(rotation.ID)).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if !consumed.Valid || consumed.Micros != created.Micros {
		t.Fatalf("credential consumption instant: %v", consumed)
	}

	// The unregistered credential from the refused rotation stays unconsumed.
	var unconsumed bool
	if err = runtime.QueryRow(ctx, `SELECT consumed_at IS NULL FROM privd_attestation_enrollment_credentials WHERE id=?`, UUIDBytes(third.ID)).Scan(&unconsumed); err != nil {
		t.Fatal(err)
	}
	if !unconsumed {
		t.Fatal("refused rotation consumed the credential")
	}

	if err = service.Revoke(ctx, privdattestation.RevokeRequest{NodeID: node, IdentityID: identity, SessionID: session, KeyID: second, RequestID: uuid.NewString(), Reason: "compromised"}); err != nil {
		t.Fatal(err)
	}
	if err = service.Revoke(ctx, privdattestation.RevokeRequest{NodeID: node, IdentityID: identity, SessionID: session, KeyID: second, RequestID: uuid.NewString(), Reason: "compromised"}); !errors.Is(err, privdattestation.ErrKeyNotFound) {
		t.Fatalf("double revocation: %v", err)
	}
	var revoked, revokedUntil value.Timestamp
	var state string
	if err = runtime.QueryRow(ctx, `SELECT state,revoked_at,valid_until FROM node_privd_attestation_keys WHERE key_id=?`, second).Scan(&state, &revoked, &revokedUntil); err != nil {
		t.Fatal(err)
	}
	if state != "revoked" || !revoked.Valid || revoked.Micros < activated.Micros || revokedUntil.Micros > revoked.Micros {
		t.Fatalf("revocation storage: %s %v %v", state, revoked, revokedUntil)
	}
	var revision uint64
	if err = runtime.QueryRow(ctx, `SELECT authorization_revision FROM nodes WHERE id=?`, UUIDBytes(node)).Scan(&revision); err != nil || revision != 4 {
		t.Fatalf("authorization revisions: %d %v", revision, err)
	}

	// Telemetry ingestion reads the converted columns through the same typed
	// contract, including the predecessor's bounded validity.
	err = database.Within(ctx, runtime, database.ReadCommitted, func(tx database.Tx) error {
		store, err := telemetrywrite.From(tx)
		if err != nil {
			return err
		}
		key, err := store.AttestationKey(ctx, node, first)
		if err != nil {
			return err
		}
		if key.State != "active" {
			t.Fatalf("telemetry key state: %s", key.State)
		}
		want, err := firstActivated.Time()
		if err != nil {
			return err
		}
		if !key.ActivatedAt.Equal(want) {
			t.Fatalf("telemetry activation: %v", key.ActivatedAt)
		}
		// The predecessor's bounded validity comes from the rotation instant.
		successorActivated, err := secondActivated.Time()
		if err != nil {
			return err
		}
		if key.ValidUntil == nil || key.ValidUntil.Sub(successorActivated) != 24*time.Hour {
			t.Fatalf("telemetry key times: %v %v", key.ActivatedAt, key.ValidUntil)
		}
		key, err = store.AttestationKey(ctx, node, second)
		if err != nil {
			return err
		}
		if key.ValidUntil == nil || !key.ValidUntil.After(key.ActivatedAt) {
			t.Fatalf("revoked key validity: %v %v", key.ActivatedAt, key.ValidUntil)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.ValidateSchema(ctx, 34); err != nil {
		t.Fatal(err)
	}
}

func registerRaw(t *testing.T, service *privdattestation.Service, node uuid.UUID, credential privdattestation.Credential) (string, error) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registration := &agentv1.PrivdAttestationRegistrationV1{
		Version:                 agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1,
		NodeId:                  node[:],
		PrivdAttestationKeyId:   privdattestation.PublicKeyID(public),
		PublicKey:               public,
		ControllerNonce:         credential.ControllerNonce,
		CredentialContextSha256: credential.CredentialContextSHA256,
	}
	canonical, err := privdattestation.CanonicalRegistrationV1(registration)
	if err != nil {
		t.Fatal(err)
	}
	registration.Signature = ed25519.Sign(private, canonical)
	return service.Register(context.Background(), privdattestation.RegistrationRequest{NodeID: node, Credential: credential.Value, Registration: registration, RequestID: uuid.NewString()})
}
