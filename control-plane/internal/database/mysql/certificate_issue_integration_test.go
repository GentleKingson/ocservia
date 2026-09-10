package mysql

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

type certificateIssuerFixture struct {
	key       *ecdsa.PrivateKey
	root      *x509.Certificate
	der       []byte
	fail      bool
	afterSign func()
	calls     int
}

func (s *certificateIssuerFixture) Sign(_ context.Context, r certificates.SignRequest) (certificates.SignResult, error) {
	s.calls++
	if s.fail {
		return certificates.SignResult{}, errors.New("isolated signer outage")
	}
	csr, err := x509.ParseCertificateRequest(r.CSRDER)
	if err != nil {
		return certificates.SignResult{}, err
	}
	now := time.Now()
	leaf := &x509.Certificate{SerialNumber: new(big.Int).SetBytes(r.CertificateID[:]), Subject: csr.Subject, DNSNames: csr.DNSNames, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, leaf, s.root, csr.PublicKey, s.key)
	if err != nil {
		return certificates.SignResult{}, err
	}
	if s.afterSign != nil {
		s.afterSign()
	}
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.der})...)
	return certificates.SignResult{CertificateChainPEM: chain}, nil
}
func (*certificateIssuerFixture) Revoke(context.Context, certificates.RevokeSignerRequest) error {
	return nil
}

func (*certificateIssuerFixture) Seal(_ context.Context, _ uuid.UUID, purpose agentv1.SealedSecretPurpose, plaintext []byte) (*agentv1.SealedSecretV1, error) {
	digest := sha256.Sum256(plaintext)
	return &agentv1.SealedSecretV1{Version: agentv1.SealedSecretVersion_SEALED_SECRET_VERSION_V1, Purpose: purpose, KeyId: "fixture-p12-key", Ciphertext: append(digest[:], digest[:]...)}, nil
}

func TestRealCertificateIssuanceTransactions(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	id := func() uuid.UUID { return uuid.Must(uuid.NewV7()) }
	workspace, node, actor, approver, session, credential, operation, certificate, approval := id(), id(), id(), id(), id(), id(), id(), id(), id()
	now := time.Now().UTC().Truncate(time.Microsecond)
	stamp, expires := fixtureTimestamp(t, now), fixtureTimestamp(t, now.Add(time.Hour))
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'issue',?,?,?)`, UUIDBytes(workspace), workspace.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	run(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES(?,?,'issue','offline',?,?)`, UUIDBytes(node), UUIDBytes(workspace), fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	for _, identity := range []uuid.UUID{actor, approver} {
		run(`INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES(?,'issue',?,?,?)`, UUIDBytes(identity), identity.String(), stamp, stamp)
	}
	run(`INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)VALUES(?,?,?,?)`, UUIDBytes(session), UUIDBytes(actor), expires, stamp)
	digest := sha256.Sum256([]byte(certificate.String()))
	run(`INSERT INTO privd_attestation_enrollment_credentials(id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,consumed_at,created_by_identity_id,created_by_session_id,created_at)VALUES(?,?,?,?,?,?,?,?,?,?)`, UUIDBytes(credential), UUIDBytes(node), digest[:], digest[:], digest[:], expires, stamp, UUIDBytes(actor), UUIDBytes(session), stamp)
	keyID := "ed25519-sha256:" + strings.Repeat("ab", 32)
	infinity := value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	run(`INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,valid_until,registration_credential_id)VALUES(?,?,'ed25519',?,'active',?,?,?,?,?)`, UUIDBytes(node), keyID, digest[:], stamp, stamp, stamp, infinity, UUIDBytes(credential))
	run(`INSERT INTO operations(id,workspace_id,node_id,state,request_id,created_at,updated_at)VALUES(?,?,?,'succeeded',?,?,?)`, UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), operation.String(), stamp, stamp)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "issue.example.test"}, DNSNames: []string{"issue.example.test"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	csrHash := sha256.Sum256(csr)
	subject, err := privdattestation.RequestedSubjectDigest(&agentv1.CertificateCsr{CertificateId: certificate[:], CommonName: "issue.example.test", DnsNames: []string{"issue.example.test"}, KeyBits: 2048})
	if err != nil {
		t.Fatal(err)
	}
	run(`INSERT INTO certificates(id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,csr_der,csr_receipt_verified_at,csr_receipt_sha256,csr_privd_attestation_key_id,csr_effect_record_id,csr_der_sha256,csr_requested_subject_sha256,created_at,updated_at)VALUES(?,?,?,?,'issue.example.test','["issue.example.test"]',2048,'csr_ready',?,?,?,?,?,?,?,?,?)`, UUIDBytes(certificate), UUIDBytes(workspace), UUIDBytes(node), UUIDBytes(operation), csr, infinity, digest[:], keyID, digest[:], csrHash[:], subject, stamp, stamp)
	if err := owner.GrantTestPrivileges(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, err := driver.ParseDSN(options.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Passwd = "ocservia_app", "pr02-runtime-test-only"
	options.DSN = cfg.FormatDSN()
	backend, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated issuer"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	signer := &certificateIssuerFixture{key: caKey, root: root, der: rootDER, fail: true}
	commandSigner, err := commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	service := certificates.NewBackend(backend, operations.NewBackend(backend, 50, commandSigner), signer, signer, nil, nil)
	boundWorkspace, boundNode, hash, summary, err := service.ApprovalBinding(ctx, certificate)
	if err != nil || boundWorkspace != workspace || boundNode != node {
		t.Fatal("approval binding", err)
	}
	jsonSummary, err := value.ParseJSONB(summary)
	if err != nil {
		t.Fatal(err)
	}
	run(`INSERT INTO approval_requests(id,workspace_id,requester_id,approver_id,action,resource_type,resource_id,reason,status,expires_at,created_at,request_hash,request_summary)VALUES(?,?,?,?,'certificate.issue','certificate',?,'isolated issuance','approved',?,?,?,?)`, UUIDBytes(approval), UUIDBytes(workspace), UUIDBytes(actor), UUIDBytes(approver), UUIDBytes(certificate), expires, stamp, hash, jsonSummary)
	request := certificates.IssueRequest{CertificateID: certificate, ApprovalID: approval, ActorIdentityID: actor, ActorSessionID: session, Reason: "isolated issuance", RequestID: "issue-1"}
	if _, err := service.Issue(ctx, request); !errors.Is(err, certificates.ErrSignerUnavailable) {
		t.Fatal("signer failure", err)
	}
	got, err := service.Get(ctx, certificate)
	if err != nil || got.State != "signer_unavailable" {
		t.Fatal("durable signer failure", got.State, err)
	}
	signer.fail = false
	signer.afterSign = func() {
		run(`UPDATE node_privd_attestation_keys SET state='revoked',revoked_at=? WHERE node_id=? AND key_id=?`, stamp, UUIDBytes(node), keyID)
	}
	if _, err := service.Issue(ctx, request); !errors.Is(err, certificates.ErrNotReady) {
		t.Fatal("revoked key during signing accepted", err)
	}
	got, err = service.Get(ctx, certificate)
	if err != nil || got.State != "signing" {
		t.Fatal("race committed issued state", got.State, err)
	}
	run(`UPDATE node_privd_attestation_keys SET state='active',revoked_at=NULL WHERE node_id=? AND key_id=?`, UUIDBytes(node), keyID)
	signer.afterSign = nil
	got, err = service.Issue(ctx, request)
	if err != nil || got.State != "issued" || got.NotAfter == nil {
		t.Fatal("retry issuance", got.State, err)
	}
	var intents, successes int
	if err := owner.QueryRow(ctx, `SELECT SUM(result='intent'),SUM(result='succeeded') FROM audit_events WHERE resource_id=? AND action='certificate.issue'`, UUIDBytes(certificate)).Scan(&intents, &successes); err != nil || intents != 1 || successes != 1 {
		t.Fatal("issuance audit", intents, successes, err)
	}
	secret, err := service.CreateSecretRef(ctx, certificates.SecretRefRequest{WorkspaceID: workspace, ActorID: actor, SessionID: session, Provider: "isolated", KeyPath: "certificates/key", Version: "1", Reason: "isolated create", RequestID: "secret-create"})
	if err != nil {
		t.Fatal(err)
	}
	if secret.RotatedAt != nil || !secret.CreatedAt.Valid {
		t.Fatal("secret creation", secret)
	}
	negative := value.Timestamp{Valid: true, Micros: value.NegativeInfinity}
	run(`UPDATE secret_provider_refs SET created_at=?,updated_at=?,rotated_at=? WHERE id=?`, negative, infinity, infinity, UUIDBytes(secret.ID))
	read, err := service.GetSecretRef(ctx, secret.ID)
	if err != nil || read.CreatedAt != negative || read.UpdatedAt != infinity || read.RotatedAt == nil || *read.RotatedAt != infinity {
		t.Fatal("secret logical read", read, err)
	}
	rotated, err := service.RotateSecretRef(ctx, secret.ID, certificates.SecretRefRequest{ActorID: actor, SessionID: session, Version: "2", Reason: "isolated rotate", RequestID: "secret-rotate"})
	if err != nil || rotated.Version != "2" || rotated.CreatedAt != negative || rotated.RotatedAt == nil || rotated.UpdatedAt != *rotated.RotatedAt {
		t.Fatal("secret rotation", rotated, err)
	}
	if resource, err := service.SecretRefResource(ctx, secret.ID); err != nil || resource != workspace {
		t.Fatal("secret resource", resource, err)
	}
	for _, capability := range []string{"privd_result_attestation_v1", "ocserv.certificate.issue", "ocserv.certificate.revoke"} {
		run(`INSERT INTO node_capabilities(node_id,capability,approved)VALUES(?,?,true)`, UUIDBytes(node), capability)
	}
	trace := "00-" + strings.Repeat("1", 32) + "-" + strings.Repeat("2", 16) + "-01"
	create := certificates.CreateRequest{NodeID: node, ActorIdentityID: actor, ActorSessionID: session, ExpectedVersion: 1, IdempotencyKey: "common-csr", CommonName: "common.example.test", DNSNames: []string{"common.example.test"}, KeyBits: 2048, ActorID: actor.String(), Reason: "common creation", RequestID: "common-csr", Traceparent: trace}
	pending, replayed, err := service.Create(ctx, create)
	if err != nil || replayed || pending.State != "csr_pending" {
		t.Fatal("common CSR creation", pending, replayed, err)
	}
	replay, replayed, err := service.Create(ctx, create)
	if err != nil || !replayed || replay.ID != pending.ID {
		t.Fatal("common CSR replay", replay, replayed, err)
	}
	run(`INSERT INTO node_sealing_keys(node_id,purpose,version,key_id,public_key_sha256,created_at)VALUES(?,2,1,'fixture-p12-key',?,?)`, UUIDBytes(node), digest[:], fixtureTimestamp(t, now))
	artifact, exportApproval := id(), id()
	_, _, exportHash, exportSummary, err := service.ActionApprovalBinding(ctx, "certificate.private_key.export", certificate, got.Version, "certificate_p12", artifact)
	if err != nil {
		t.Fatal(err)
	}
	exportJSON, err := value.ParseJSONB(exportSummary)
	if err != nil {
		t.Fatal(err)
	}
	run(`INSERT INTO approval_requests(id,workspace_id,requester_id,approver_id,action,resource_type,resource_id,reason,status,expires_at,created_at,request_hash,request_summary)VALUES(?,?,?,?,'certificate.private_key.export','certificate',?,'isolated export','approved',?,?,?,?)`, UUIDBytes(exportApproval), UUIDBytes(workspace), UUIDBytes(actor), UUIDBytes(approver), UUIDBytes(certificate), expires, stamp, exportHash, exportJSON)
	export := certificates.P12Request{CertificateID: certificate, ApprovalID: exportApproval, ArtifactRequestID: artifact, ActorIdentityID: actor, ActorSessionID: session, CertificateVersion: got.Version, ExpectedVersion: 1, IdempotencyKey: "common-export", Reason: "isolated export", RequestID: "common-export", Traceparent: trace}
	grant, replayed, err := service.CreateP12(ctx, export)
	if err != nil || replayed || grant.ArtifactID != artifact || grant.Password == "" || grant.DownloadToken == "" {
		t.Fatal("common P12 creation", grant.ArtifactID, replayed, err)
	}
	grantReplay, replayed, err := service.CreateP12(ctx, export)
	if err != nil || !replayed || grantReplay.ArtifactID != artifact || grantReplay.Operation.ID != grant.Operation.ID || grantReplay.Password != "" || grantReplay.DownloadToken != "" {
		t.Fatal("common P12 replay", grantReplay.ArtifactID, replayed, err)
	}
	_, _, revokeHash, revokeSummary, err := service.ActionApprovalBinding(ctx, "certificate.revoke", certificate, got.Version, "isolated revoke", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	revokeJSON, err := value.ParseJSONB(revokeSummary)
	if err != nil {
		t.Fatal(err)
	}
	revokeApproval := id()
	run(`INSERT INTO approval_requests(id,workspace_id,requester_id,approver_id,action,resource_type,resource_id,reason,status,expires_at,created_at,request_hash,request_summary)VALUES(?,?,?,?,'certificate.revoke','certificate',?,'isolated revoke','approved',?,?,?,?)`, UUIDBytes(revokeApproval), UUIDBytes(workspace), UUIDBytes(actor), UUIDBytes(approver), UUIDBytes(certificate), expires, stamp, revokeHash, revokeJSON)
	revocation, replayed, err := service.Revoke(ctx, certificates.RevokeRequest{CertificateID: certificate, ApprovalID: revokeApproval, ActorIdentityID: actor, ActorSessionID: session, CertificateVersion: got.Version, ExpectedVersion: 1, IdempotencyKey: "common-revoke", Reason: "isolated revoke", RequestID: "common-revoke", Traceparent: trace})
	if err != nil || replayed || revocation.State != "queued" {
		t.Fatal("common revocation", revocation, replayed, err)
	}
	revoking, err := service.Get(ctx, certificate)
	if err != nil || revoking.State != "revoking" {
		t.Fatal("common revocation state", revoking, err)
	}
	var artifactState string
	if err := owner.QueryRow(ctx, `SELECT state FROM artifact_operations WHERE id=?`, UUIDBytes(artifact)).Scan(&artifactState); err != nil || artifactState != "revoked" {
		t.Fatal("revocation did not revoke pending export", artifactState, err)
	}
	var available, commandExpires value.Timestamp
	if err := owner.QueryRow(ctx, `SELECT o.available_at,c.expires_at FROM outbox_events o JOIN commands c ON c.id=o.command_id WHERE c.operation_id=?`, UUIDBytes(uuid.MustParse(revocation.ID))).Scan(&available, &commandExpires); err != nil || !available.Valid || available.Micros >= commandExpires.Micros {
		t.Fatal("revocation outbox release", available, commandExpires, err)
	}
}
