package privdattestation

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestRootCredentialRegistrationReplayRotationAndRevocationIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	t.Setenv("OCSERV_ENVIRONMENT", "test")
	if os.Getenv("OCSERV_TEST_AUDIT_EVENT_KEY_HEX") == "" {
		t.Setenv("OCSERV_AUDIT_EVENT_KEY_ID", "test-attestation-v1")
		t.Setenv("OCSERV_TEST_AUDIT_EVENT_KEY_HEX", "1111111111111111111111111111111111111111111111111111111111111111")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	workspaceID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	identityID, sessionID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := time.Now().UTC().Truncate(time.Microsecond)
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'privd registration',$2,$3,$3)`, []any{workspaceID, "privd-registration-" + workspaceID.String(), now}},
		{`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',$4,$4)`, []any{nodeID, workspaceID, "node-" + nodeID.String(), now}},
		{`INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'https://idp.example',$2,$3,$3)`, []any{identityID, "security-admin-" + identityID.String(), now}},
		{`INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,$3,$4)`, []any{sessionID, identityID, now.Add(time.Hour), now}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	service := New(pool)
	service.now = func() time.Time { return now }
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_capabilities WHERE node_id=$1 AND capability=$2`, nodeID, AttestationCapability)
		_, _ = pool.Exec(context.Background(), `DELETE FROM node_privd_attestation_keys WHERE node_id=$1`, nodeID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM privd_attestation_enrollment_credentials WHERE node_id=$1`, nodeID)
	}()
	credential := createTestCredential(t, ctx, service, nodeID, identityID, sessionID, "first")
	firstKey := ed25519.NewKeyFromSeed(makeRepeated(0x31, ed25519.SeedSize))
	registration := signedRegistration(t, nodeID, firstKey, credential)

	wrongNonce := proto.Clone(registration).(*agentv1.PrivdAttestationRegistrationV1)
	wrongNonce.ControllerNonce = makeRepeated(0xff, 32)
	canonical, err := CanonicalRegistrationV1(wrongNonce)
	if err != nil {
		t.Fatal(err)
	}
	wrongNonce.Signature = ed25519.Sign(firstKey, canonical)
	if _, err := service.Register(ctx, RegistrationRequest{NodeID: nodeID, Credential: credential.Value, Registration: wrongNonce, RequestID: "wrong-nonce"}); !errors.Is(err, ErrCredential) {
		t.Fatalf("wrong Controller nonce error=%v", err)
	}

	firstID, err := service.Register(ctx, RegistrationRequest{NodeID: nodeID, Credential: credential.Value, Registration: registration, RequestID: "register-first"})
	if err != nil || firstID != PublicKeyID(firstKey.Public().(ed25519.PublicKey)) {
		t.Fatalf("first key registration id=%q err=%v", firstID, err)
	}
	if _, err := service.Register(ctx, RegistrationRequest{NodeID: nodeID, Credential: credential.Value, Registration: registration, RequestID: "credential-replay"}); !errors.Is(err, ErrCredential) {
		t.Fatalf("consumed credential replay error=%v", err)
	}

	secondCredential := createTestCredential(t, ctx, service, nodeID, identityID, sessionID, "rotation")
	secondKey := ed25519.NewKeyFromSeed(makeRepeated(0x32, ed25519.SeedSize))
	secondID, err := service.Register(ctx, RegistrationRequest{NodeID: nodeID, Credential: secondCredential.Value, Registration: signedRegistration(t, nodeID, secondKey, secondCredential), RequestID: "register-second"})
	if err != nil {
		t.Fatal(err)
	}
	var active, capabilities, consumed int
	var firstSuccessor string
	var firstValidUntil *time.Time
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM node_privd_attestation_keys WHERE node_id=$1 AND state='active'),
			(SELECT count(*) FROM node_capabilities WHERE node_id=$1 AND capability=$2 AND approved=true),
		(SELECT count(*) FROM privd_attestation_enrollment_credentials WHERE node_id=$1 AND consumed_at IS NOT NULL),
		(SELECT successor_key_id FROM node_privd_attestation_keys WHERE node_id=$1 AND key_id=$3),
		(SELECT valid_until FROM node_privd_attestation_keys WHERE node_id=$1 AND key_id=$3)`,
		nodeID, AttestationCapability, firstID).Scan(&active, &capabilities, &consumed, &firstSuccessor, &firstValidUntil); err != nil {
		t.Fatal(err)
	}
	if active != 2 || capabilities != 1 || consumed != 2 || firstSuccessor != secondID || firstValidUntil == nil || !firstValidUntil.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("rotation state active=%d capability=%d consumed=%d successor=%q valid_until=%v", active, capabilities, consumed, firstSuccessor, firstValidUntil)
	}

	if err := service.Revoke(ctx, RevokeRequest{NodeID: nodeID, IdentityID: identityID, SessionID: sessionID, KeyID: firstID, RequestID: "revoke-first", Reason: "rotation complete"}); err != nil {
		t.Fatal(err)
	}
	var state string
	var revokedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT state,revoked_at FROM node_privd_attestation_keys WHERE node_id=$1 AND key_id=$2`, nodeID, firstID).Scan(&state, &revokedAt); err != nil || state != "revoked" || !revokedAt.Equal(now) {
		t.Fatalf("revoked key state=%q at=%v err=%v", state, revokedAt, err)
	}
}

func createTestCredential(t *testing.T, ctx context.Context, service *Service, nodeID, identityID, sessionID uuid.UUID, suffix string) Credential {
	t.Helper()
	credential, err := service.CreateCredential(ctx, CredentialRequest{
		NodeID: nodeID, IdentityID: identityID, SessionID: sessionID, TTL: 15 * time.Minute,
		RequestID: "credential-" + suffix, Reason: "root provisioning integration",
	})
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

func signedRegistration(t *testing.T, nodeID uuid.UUID, privateKey ed25519.PrivateKey, credential Credential) *agentv1.PrivdAttestationRegistrationV1 {
	t.Helper()
	publicKey := privateKey.Public().(ed25519.PublicKey)
	registration := &agentv1.PrivdAttestationRegistrationV1{
		Version: agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1,
		NodeId:  nodeID[:], PrivdAttestationKeyId: PublicKeyID(publicKey), PublicKey: publicKey,
		ControllerNonce: credential.ControllerNonce, CredentialContextSha256: credential.CredentialContextSHA256,
	}
	canonical, err := CanonicalRegistrationV1(registration)
	if err != nil {
		t.Fatal(err)
	}
	registration.Signature = ed25519.Sign(privateKey, canonical)
	return registration
}
