package certificates

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixturePKI struct {
	key               *rsa.PrivateKey
	certificate       *x509.Certificate
	revoked           bool
	revokeUnavailable bool
	unavailable       bool
}

func newFixturePKI(t *testing.T) *fixturePKI {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "I17 Fixture CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &fixturePKI{key: key, certificate: certificate}
}
func (f *fixturePKI) Sign(_ context.Context, request SignRequest) (SignResult, error) {
	if f.unavailable {
		return SignResult{}, errors.New("fixture signer unavailable")
	}
	csr, err := x509.ParseCertificateRequest(request.CSRDER)
	if err != nil {
		return SignResult{}, err
	}
	template := &x509.Certificate{SerialNumber: new(big.Int).SetBytes(request.CertificateID[:]), Subject: csr.Subject, DNSNames: csr.DNSNames, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(2 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, f.certificate, csr.PublicKey, f.key)
	if err != nil {
		return SignResult{}, err
	}
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.certificate.Raw})...)
	return SignResult{CertificateChainPEM: chain}, nil
}
func (f *fixturePKI) Revoke(_ context.Context, _ RevokeSignerRequest) error {
	if f.revokeUnavailable {
		return errors.New("fixture revocation unavailable")
	}
	f.revoked = true
	return nil
}
func (f *fixturePKI) Seal(_ context.Context, _ uuid.UUID, plaintext []byte) ([]byte, string, error) {
	digest := sha256.Sum256(plaintext)
	return append(digest[:], digest[:]...), "fixture-node-key", nil
}

type fixtureArtifacts struct{ data []byte }

func (f fixtureArtifacts) FetchArtifact(_ context.Context, _ uuid.UUID, _ uuid.UUID, max int64) (io.ReadCloser, error) {
	if int64(len(f.data)) > max {
		return nil, errors.New("too large")
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func TestCertificateIssueArtifactAndRevokeIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner := pool
	if ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL"); ownerURL != "" {
		owner, err = pgxpool.New(ctx, ownerURL)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close()
	}
	workspaceID, nodeID, requesterID, approverID, requesterSession, approverSession := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, setup := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'I17 test',$2,now(),now())`, []any{workspaceID, "i17-" + workspaceID.String()}},
		{`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)VALUES($1,$2,$3,'offline',1,now(),now())`, []any{nodeID, workspaceID, "node-" + nodeID.String()}},
		{`INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES($1,'test',$2,now(),now()),($3,'test',$4,now(),now())`, []any{requesterID, "i17-requester-" + requesterID.String(), approverID, "i17-approver-" + approverID.String()}},
		{`INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,'ocserv.certificate.issue',true),($1,'ocserv.certificate.revoke',true)`, []any{nodeID}},
	} {
		if _, err = pool.Exec(ctx, setup.query, setup.args...); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		if err := cleanupCertificateIntegration(context.Background(), owner, workspaceID); err != nil {
			t.Error(err)
		}
	}()
	pki := newFixturePKI(t)
	artifactBytes := []byte("encrypted-p12-fixture")
	service := NewWithDependencies(pool, operations.New(pool), pki, pki, fixtureArtifacts{data: artifactBytes})
	certificate, replayed, err := service.Create(ctx, CreateRequest{NodeID: nodeID, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-csr", CommonName: "node.example.test", DNSNames: []string{"node.example.test"}, KeyBits: 2048, ActorID: requesterID.String(), Reason: "issue node certificate", RequestID: "i17-csr-request", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if err != nil || replayed {
		t.Fatalf("create certificate replay=%v err=%v", replayed, err)
	}
	replayedCertificate, replayed, err := service.Create(ctx, CreateRequest{NodeID: nodeID, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-csr", CommonName: "node.example.test", DNSNames: []string{"node.example.test"}, KeyBits: 2048, ActorID: requesterID.String(), Reason: "issue node certificate", RequestID: "i17-csr-request", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if err != nil || !replayed || replayedCertificate.ID != certificate.ID {
		t.Fatalf("replay certificate=%s want=%s replay=%v err=%v", replayedCertificate.ID, certificate.ID, replayed, err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "node.example.test"}, DNSNames: []string{"node.example.test"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	publicHash := sha256.Sum256(publicDER)
	if _, err = pool.Exec(ctx, `UPDATE certificates SET state='csr_ready',csr_der=$2,public_key_sha256=$3 WHERE id=$1`, certificate.ID, csrDER, publicHash[:]); err != nil {
		t.Fatal(err)
	}
	_, _, bindingHash, summary, err := service.ApprovalBinding(ctx, certificate.ID)
	if err != nil {
		t.Fatal(err)
	}
	approvalService := approvals.New(pool)
	approval, err := approvalService.Create(ctx, approvals.Request{WorkspaceID: workspaceID, RequesterID: requesterID, ResourceID: certificate.ID, Action: "certificate.issue", ResourceType: "certificate", Reason: "approve certificate", TTL: time.Hour, SessionID: requesterSession, RequestID: "approval-request", RequestHash: bindingHash, RequestSummary: summary})
	if err != nil {
		t.Fatal(err)
	}
	approval, err = approvalService.Approve(ctx, approvals.Decision{ApprovalID: approval.ID, ApproverID: approverID, SessionID: approverSession, Reason: "independent review", RequestID: "approval-decision", ExpectedRequestHash: approval.RequestHash})
	if err != nil {
		t.Fatal(err)
	}
	pki.unavailable = true
	if _, err := service.Issue(ctx, IssueRequest{CertificateID: certificate.ID, ApprovalID: approval.ID, ActorIdentityID: requesterID, ActorSessionID: requesterSession, Reason: "issue node certificate", RequestID: "issue-unavailable"}); !errors.Is(err, ErrSignerUnavailable) {
		t.Fatalf("signer unavailable err=%v", err)
	}
	var issueState, approvalState string
	if err := pool.QueryRow(ctx, `SELECT c.state,a.status FROM certificates c JOIN approval_requests a ON a.id=c.issue_approval_id WHERE c.id=$1`, certificate.ID).Scan(&issueState, &approvalState); err != nil || issueState != "signer_unavailable" || approvalState != "consumed" {
		t.Fatalf("durable issue intent state=%q approval=%q err=%v", issueState, approvalState, err)
	}
	pki.unavailable = false
	issued, err := service.Issue(ctx, IssueRequest{CertificateID: certificate.ID, ApprovalID: approval.ID, ActorIdentityID: requesterID, ActorSessionID: requesterSession, Reason: "issue node certificate", RequestID: "issue-request"})
	if err != nil || issued.State != "issued" || issued.SerialNumber == "" {
		t.Fatalf("issued=%+v err=%v", issued, err)
	}
	grant, replayed, err := service.CreateP12(ctx, P12Request{CertificateID: certificate.ID, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-p12", Reason: "export certificate", RequestID: "p12-request", Traceparent: "00-1123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if err != nil || replayed || grant.Password == "" || grant.DownloadToken == "" {
		t.Fatalf("grant=%+v replay=%v err=%v", grant, replayed, err)
	}
	replayedGrant, replayed, err := service.CreateP12(ctx, P12Request{CertificateID: certificate.ID, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-p12", Reason: "export certificate", RequestID: "p12-request", Traceparent: "00-1123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if err != nil || !replayed || replayedGrant.ArtifactID != grant.ArtifactID || replayedGrant.Password != "" || replayedGrant.DownloadToken != "" {
		t.Fatalf("replayed grant=%+v replay=%v err=%v", replayedGrant, replayed, err)
	}
	digest := sha256.Sum256(artifactBytes)
	if _, err = pool.Exec(ctx, `UPDATE artifact_operations SET state='ready',content_sha256=$2,content_size=$3 WHERE id=$1`, grant.ArtifactID, digest[:], len(artifactBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err = service.OpenArtifact(ctx, grant.ArtifactID, strings.Repeat("x", 43)); !errors.Is(err, ErrArtifactDenied) {
		t.Fatalf("wrong artifact token err=%v", err)
	}
	download, err := service.OpenArtifact(ctx, grant.ArtifactID, grant.DownloadToken)
	if err != nil {
		t.Fatal(err)
	}
	received, err := io.ReadAll(download.Reader)
	download.Reader.Close()
	if err != nil || !bytes.Equal(received, artifactBytes) {
		t.Fatalf("download=%q err=%v", received, err)
	}
	if err = service.AbortArtifact(ctx, grant.ArtifactID); err != nil {
		t.Fatal(err)
	}
	download, err = service.OpenArtifact(ctx, grant.ArtifactID, grant.DownloadToken)
	if err != nil {
		t.Fatalf("resume interrupted artifact: %v", err)
	}
	_, _ = io.Copy(io.Discard, download.Reader)
	_ = download.Reader.Close()
	if err = service.CompleteArtifact(ctx, grant.ArtifactID, digest[:], int64(len(artifactBytes)), requesterID, requesterSession, "artifact-download"); err != nil {
		t.Fatal(err)
	}
	if _, err = service.OpenArtifact(ctx, grant.ArtifactID, grant.DownloadToken); !errors.Is(err, ErrArtifactDenied) {
		t.Fatalf("second download err=%v", err)
	}
	expiredGrant, replayed, err := service.CreateP12(ctx, P12Request{CertificateID: certificate.ID, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-p12-expired", Reason: "expired export", RequestID: "p12-expired-request", Traceparent: "00-3123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if err != nil || replayed {
		t.Fatalf("expired fixture grant=%+v replay=%v err=%v", expiredGrant, replayed, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE artifact_operations SET state='ready',content_sha256=$2,content_size=$3,expires_at=now()-interval '1 second' WHERE id=$1`, expiredGrant.ArtifactID, digest[:], len(artifactBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err = service.OpenArtifact(ctx, expiredGrant.ArtifactID, expiredGrant.DownloadToken); !errors.Is(err, ErrArtifactDenied) {
		t.Fatalf("expired artifact err=%v", err)
	}
	if err := service.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	hashGrant, replayed, err := service.CreateP12(ctx, P12Request{CertificateID: certificate.ID, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-p12-hash", Reason: "integrity export", RequestID: "p12-hash-request", Traceparent: "00-4123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if err != nil || replayed {
		t.Fatalf("hash fixture grant=%+v replay=%v err=%v", hashGrant, replayed, err)
	}
	wrongDigest := sha256.Sum256([]byte("different artifact"))
	if _, err = pool.Exec(ctx, `UPDATE artifact_operations SET state='ready',content_sha256=$2,content_size=$3 WHERE id=$1`, hashGrant.ArtifactID, wrongDigest[:], len(artifactBytes)); err != nil {
		t.Fatal(err)
	}
	download, err = service.OpenArtifact(ctx, hashGrant.ArtifactID, hashGrant.DownloadToken)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, download.Reader)
	_ = download.Reader.Close()
	if err := service.CompleteArtifact(ctx, hashGrant.ArtifactID, digest[:], int64(len(artifactBytes)), requesterID, requesterSession, "artifact-hash-mismatch"); !errors.Is(err, ErrArtifactDenied) {
		t.Fatalf("artifact hash mismatch err=%v", err)
	}
	if err := service.AbortArtifact(ctx, hashGrant.ArtifactID); err != nil {
		t.Fatal(err)
	}
	secretRef, err := service.CreateSecretRef(ctx, SecretRefRequest{WorkspaceID: workspaceID, ActorID: approverID, SessionID: approverSession, Provider: "vault", KeyPath: "production/ocserv/tls", Version: "version-1", Reason: "register external reference", RequestID: "secret-create"})
	if err != nil || secretRef.State != "active" {
		t.Fatalf("secret ref=%+v err=%v", secretRef, err)
	}
	rotated, err := service.RotateSecretRef(ctx, secretRef.ID, SecretRefRequest{ActorID: approverID, SessionID: approverSession, Version: "version-2", Reason: "rotate external reference", RequestID: "secret-rotate"})
	if err != nil || rotated.Version != "version-2" || rotated.RotatedAt == nil {
		t.Fatalf("rotated ref=%+v err=%v", rotated, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE certificates SET not_after=now()-interval '1 second' WHERE id=$1`, certificate.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if expired, err := service.Get(ctx, certificate.ID); err != nil || expired.State != "expired" {
		t.Fatalf("expired certificate=%+v err=%v", expired, err)
	}
	if _, _, err := service.CreateP12(ctx, P12Request{CertificateID: certificate.ID, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-p12-after-certificate-expiry", Reason: "reject expired certificate", RequestID: "p12-after-certificate-expiry"}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("expired certificate P12 err=%v", err)
	}
	pki.revokeUnavailable = true
	op, replayed, err := service.Revoke(ctx, RevokeRequest{CertificateID: certificate.ID, ActorIdentityID: approverID, ActorSessionID: approverSession, ExpectedVersion: 1, IdempotencyKey: "i17-revoke", Reason: "retire certificate", RequestID: "revoke-request", Traceparent: "00-2123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if !errors.Is(err, ErrSignerUnavailable) || replayed || op.ID == "" {
		t.Fatalf("durable revoke=%+v replay=%v err=%v", op, replayed, err)
	}
	var revokeState string
	var held bool
	if err := pool.QueryRow(ctx, `SELECT c.state,o.available_at>now() FROM certificates c JOIN operations p ON p.workspace_id=c.workspace_id AND p.idempotency_key='i17-revoke' JOIN outbox_events o ON o.command_id=p.command_id WHERE c.id=$1`, certificate.ID).Scan(&revokeState, &held); err != nil || revokeState != "revocation_unknown" || !held {
		t.Fatalf("durable revoke intent state=%q held=%v err=%v", revokeState, held, err)
	}
	pki.revokeUnavailable = false
	op, replayed, err = service.Revoke(ctx, RevokeRequest{CertificateID: certificate.ID, ActorIdentityID: approverID, ActorSessionID: approverSession, ExpectedVersion: 1, IdempotencyKey: "i17-revoke", Reason: "retire certificate", RequestID: "revoke-request", Traceparent: "00-2123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if err != nil || !replayed || op.ID == "" || !pki.revoked {
		t.Fatalf("revoke=%+v replay=%v external=%v err=%v", op, replayed, pki.revoked, err)
	}
	if err := pool.QueryRow(ctx, `SELECT available_at<=now() FROM outbox_events WHERE command_id=(SELECT command_id FROM operations WHERE id=$1::uuid)`, op.ID).Scan(&held); err != nil || !held {
		t.Fatalf("released revoke outbox=%v err=%v", held, err)
	}
}

func cleanupCertificateIntegration(ctx context.Context, pool *pgxpool.Pool, workspaceID uuid.UUID) error {
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only`); err != nil {
		return err
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only`)
	}()
	statements := []string{`DELETE FROM artifact_operations WHERE workspace_id=$1`, `DELETE FROM certificates WHERE workspace_id=$1`, `DELETE FROM secret_provider_refs WHERE workspace_id=$1`, `DELETE FROM agent_command_results WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM node_command_leases WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM command_attempts WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id=$1)`, `DELETE FROM audit_events WHERE workspace_id=$1`, `DELETE FROM security_alerts WHERE workspace_id=$1`, `DELETE FROM approval_requests WHERE workspace_id=$1`, `DELETE FROM commands WHERE workspace_id=$1`, `DELETE FROM operations WHERE workspace_id=$1`, `DELETE FROM node_capabilities WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, `DELETE FROM nodes WHERE workspace_id=$1`, `DELETE FROM identities WHERE issuer='test' AND subject LIKE 'i17-%'`, `DELETE FROM workspaces WHERE id=$1`}
	for _, statement := range statements {
		args := []any(nil)
		if strings.Contains(statement, "$1") {
			args = append(args, workspaceID)
		}
		if _, err := pool.Exec(ctx, statement, args...); err != nil {
			return err
		}
	}
	return nil
}
