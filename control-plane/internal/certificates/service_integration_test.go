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

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
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
func (f *fixturePKI) Seal(_ context.Context, _ uuid.UUID, purpose agentv1.SealedSecretPurpose, plaintext []byte) (*agentv1.SealedSecretV1, error) {
	digest := sha256.Sum256(plaintext)
	return &agentv1.SealedSecretV1{
		Version:    agentv1.SealedSecretVersion_SEALED_SECRET_VERSION_V1,
		Purpose:    purpose,
		KeyId:      "fixture-p12-key",
		Ciphertext: append(digest[:], digest[:]...),
	}, nil
}

type fixtureArtifacts struct {
	data         []byte
	consumeCount int
	consumeErr   error
	consumed     map[string]bool
}

func (f *fixtureArtifacts) FetchArtifact(_ context.Context, grant *agentv1.ArtifactGrantV1) (io.ReadCloser, error) {
	if grant == nil || uint64(len(f.data)) > grant.GetMaxBytes() {
		return nil, errors.New("too large")
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func (f *fixtureArtifacts) ConsumeArtifact(_ context.Context, grant *agentv1.ArtifactGrantV1, digest []byte, size int64) error {
	if f.consumeErr != nil {
		return f.consumeErr
	}
	expected := sha256.Sum256(f.data)
	if grant == nil || size != int64(len(f.data)) || !bytes.Equal(digest, expected[:]) {
		return errors.New("artifact consumption mismatch")
	}
	f.consumeCount++
	if f.consumed == nil {
		f.consumed = make(map[string]bool)
	}
	f.consumed[string(grant.GetGrantId())] = true
	return nil
}

func (f *fixtureArtifacts) ConfirmArtifactConsumed(_ context.Context, grant *agentv1.ArtifactGrantV1, digest []byte, size int64) (bool, error) {
	expected := sha256.Sum256(f.data)
	if grant == nil || size != int64(len(f.data)) || !bytes.Equal(digest, expected[:]) {
		return false, errors.New("artifact confirmation mismatch")
	}
	return f.consumed[string(grant.GetGrantId())], nil
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
		{`INSERT INTO node_sealing_keys(node_id,purpose,version,key_id,public_key_sha256,created_at)VALUES($1,1,1,'fixture-user-key',decode(repeat('11',32),'hex'),now()),($1,2,1,'fixture-p12-key',decode(repeat('22',32),'hex'),now())`, []any{nodeID}},
		{`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_at)VALUES($1,$2,$3,'SecurityAdmin','node',$4,now()-interval '1 minute'),($5,$6,$3,'SecurityAdmin','node',$4,now()-interval '1 minute')`, []any{uuid.Must(uuid.NewV7()), approverID, workspaceID, nodeID, uuid.Must(uuid.NewV7()), requesterID}},
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
	var commandSeed [32]byte
	commandSeed[0] = 5
	commandSigner := commandauth.NewSignerFromSeed(commandSeed)
	artifactTransport := &fixtureArtifacts{data: artifactBytes}
	service := NewWithDependencies(pool, operations.NewWithSigner(pool, 50, commandSigner), pki, pki, artifactTransport, commandSigner)
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
	approval, err := approvalService.Create(ctx, approvals.Request{WorkspaceID: workspaceID, RequesterID: requesterID, ResourceID: certificate.ID, Action: "certificate.issue", ResourceType: "certificate", Reason: "approve certificate", TTL: time.Hour, SessionID: requesterSession, RequestID: "approval-request", RequestHash: bindingHash, RequestSummary: summary, AuthorityResources: []approvals.AuthorityResource{{WorkspaceID: workspaceID, Type: "node", ID: nodeID}}})
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
	p12ArtifactID := uuid.Must(uuid.NewV7())
	p12ApprovalID := approvedCertificateAction(t, ctx, service, approvalService, workspaceID, nodeID, certificate.ID, requesterID, requesterSession, approverID, approverSession, "certificate.private_key.export", issued.Version, "certificate_p12", p12ArtifactID)
	grant, replayed, err := service.CreateP12(ctx, P12Request{CertificateID: certificate.ID, ApprovalID: p12ApprovalID, ArtifactRequestID: p12ArtifactID, CertificateVersion: issued.Version, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-p12", Reason: "export certificate", RequestID: "p12-request", Traceparent: "00-1123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if err != nil || replayed || grant.Password == "" || grant.DownloadToken == "" {
		t.Fatalf("grant=%+v replay=%v err=%v", grant, replayed, err)
	}
	replayedGrant, replayed, err := service.CreateP12(ctx, P12Request{CertificateID: certificate.ID, ApprovalID: p12ApprovalID, ArtifactRequestID: p12ArtifactID, CertificateVersion: issued.Version, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-p12", Reason: "export certificate", RequestID: "p12-request", Traceparent: "00-1123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if err != nil || !replayed || replayedGrant.ArtifactID != grant.ArtifactID || replayedGrant.Password != "" || replayedGrant.DownloadToken != "" {
		t.Fatalf("replayed grant=%+v replay=%v err=%v", replayedGrant, replayed, err)
	}
	digest := sha256.Sum256(artifactBytes)
	if _, err = pool.Exec(ctx, `UPDATE artifact_operations SET state='ready',content_sha256=$2,content_size=$3 WHERE id=$1`, grant.ArtifactID, digest[:], len(artifactBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err = service.OpenArtifact(ctx, grant.ArtifactID, strings.Repeat("x", 43), requesterID); !errors.Is(err, ErrArtifactDenied) {
		t.Fatalf("wrong artifact token err=%v", err)
	}
	if _, err = service.OpenArtifact(ctx, grant.ArtifactID, grant.DownloadToken, approverID); !errors.Is(err, ErrArtifactDenied) {
		t.Fatalf("another SecurityAdmin used the requester's artifact token: %v", err)
	}
	download, err := service.OpenArtifact(ctx, grant.ArtifactID, grant.DownloadToken, requesterID)
	if err != nil {
		t.Fatal(err)
	}
	received, err := io.ReadAll(download.Reader)
	download.Reader.Close()
	if err != nil || !bytes.Equal(received, artifactBytes) {
		t.Fatalf("download=%q err=%v", received, err)
	}
	if err = service.AbortArtifact(ctx, grant.ArtifactID, download.GrantID); err != nil {
		t.Fatal(err)
	}
	if artifactTransport.consumeCount != 0 {
		t.Fatal("interrupted artifact transfer was consumed")
	}
	if _, err = service.OpenArtifact(ctx, grant.ArtifactID, grant.DownloadToken, requesterID); !errors.Is(err, ErrArtifactDenied) {
		t.Fatalf("active interrupted lease was reissued: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE artifact_operations SET lease_until=now()-interval '1 second',active_grant_expires_at=now()-interval '1 second' WHERE id=$1`, grant.ArtifactID); err != nil {
		t.Fatal(err)
	}
	download, err = service.OpenArtifact(ctx, grant.ArtifactID, grant.DownloadToken, requesterID)
	if err != nil {
		t.Fatalf("resume interrupted artifact: %v", err)
	}
	_, _ = io.Copy(io.Discard, download.Reader)
	_ = download.Reader.Close()
	if err = service.CompleteArtifact(ctx, grant.ArtifactID, download.GrantID, download.Grant, digest[:], int64(len(artifactBytes)), requesterID, requesterSession, "artifact-download"); err != nil {
		t.Fatal(err)
	}
	if artifactTransport.consumeCount != 1 {
		t.Fatalf("artifact consume count=%d", artifactTransport.consumeCount)
	}
	if _, err = service.OpenArtifact(ctx, grant.ArtifactID, grant.DownloadToken, requesterID); !errors.Is(err, ErrArtifactDenied) {
		t.Fatalf("second download err=%v", err)
	}
	consumeFailureArtifactID := uuid.Must(uuid.NewV7())
	consumeFailureApproval := approvedCertificateAction(t, ctx, service, approvalService, workspaceID, nodeID, certificate.ID, requesterID, requesterSession, approverID, approverSession, "certificate.private_key.export", issued.Version, "certificate_p12", consumeFailureArtifactID)
	consumeFailureGrant, _, err := service.CreateP12(ctx, P12Request{CertificateID: certificate.ID, ApprovalID: consumeFailureApproval, ArtifactRequestID: consumeFailureArtifactID, CertificateVersion: issued.Version, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-p12-consume-recovery", Reason: "test finalize recovery", RequestID: "p12-consume-recovery", Traceparent: "00-2123456789abcdef0123456789abcdef-1123456789abcdef-01"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE artifact_operations SET state='ready',content_sha256=$2,content_size=$3 WHERE id=$1`, consumeFailureGrant.ArtifactID, digest[:], len(artifactBytes)); err != nil {
		t.Fatal(err)
	}
	consumeFailureDownload, err := service.OpenArtifact(ctx, consumeFailureGrant.ArtifactID, consumeFailureGrant.DownloadToken, requesterID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, consumeFailureDownload.Reader)
	_ = consumeFailureDownload.Reader.Close()
	artifactTransport.consumeErr = errors.New("finalize unavailable")
	if err = service.CompleteArtifact(ctx, consumeFailureGrant.ArtifactID, consumeFailureDownload.GrantID, consumeFailureDownload.Grant, digest[:], int64(len(artifactBytes)), requesterID, requesterSession, "artifact-consume-failure"); err == nil {
		t.Fatal("root consumption failure was accepted")
	}
	var consumeFailureState string
	if err = pool.QueryRow(ctx, `SELECT state FROM artifact_operations WHERE id=$1`, consumeFailureGrant.ArtifactID).Scan(&consumeFailureState); err != nil || consumeFailureState != "consuming" {
		t.Fatalf("failed consumption state=%q err=%v", consumeFailureState, err)
	}
	artifactTransport.consumeErr = nil
	if err = service.Maintain(ctx); err != nil {
		t.Fatalf("recover root consumption: %v", err)
	}
	if err = pool.QueryRow(ctx, `SELECT state FROM artifact_operations WHERE id=$1`, consumeFailureGrant.ArtifactID).Scan(&consumeFailureState); err != nil || consumeFailureState != "consumed" {
		t.Fatalf("recovered consumption state=%q err=%v", consumeFailureState, err)
	}
	crashArtifactID := uuid.Must(uuid.NewV7())
	crashApprovalID := approvedCertificateAction(t, ctx, service, approvalService, workspaceID, nodeID, certificate.ID, requesterID, requesterSession, approverID, approverSession, "certificate.private_key.export", issued.Version, "certificate_p12", crashArtifactID)
	crashGrant, _, err := service.CreateP12(ctx, P12Request{CertificateID: certificate.ID, ApprovalID: crashApprovalID, ArtifactRequestID: crashArtifactID, CertificateVersion: issued.Version, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-p12-crash-recovery", Reason: "test crash recovery", RequestID: "p12-crash-recovery"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE artifact_operations SET state='ready',content_sha256=$2,content_size=$3 WHERE id=$1`, crashGrant.ArtifactID, digest[:], len(artifactBytes)); err != nil {
		t.Fatal(err)
	}
	crashDownload, err := service.OpenArtifact(ctx, crashGrant.ArtifactID, crashGrant.DownloadToken, requesterID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, crashDownload.Reader)
	_ = crashDownload.Reader.Close()
	crashGrantBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(crashDownload.Grant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE artifact_operations SET state='consuming',consume_grant=$3,consume_sha256=$4,consume_size=$5,consume_actor_id=$6,consume_session_id=$7,consume_request_id='artifact-crash-window' WHERE id=$1 AND active_grant_id=$2 AND state='leased'`, crashGrant.ArtifactID, crashDownload.GrantID, crashGrantBytes, digest[:], len(artifactBytes), requesterID, requesterSession); err != nil {
		t.Fatal(err)
	}
	if err = artifactTransport.ConsumeArtifact(ctx, crashDownload.Grant, digest[:], int64(len(artifactBytes))); err != nil {
		t.Fatal(err)
	}
	consumeCountAfterRoot := artifactTransport.consumeCount
	restartedService := NewWithDependencies(pool, operations.NewWithSigner(pool, 50, commandSigner), pki, pki, artifactTransport, commandSigner)
	restartedService.now = func() time.Time { return crashDownload.Grant.GetExpiresAt().AsTime().Add(time.Second) }
	if err = restartedService.Maintain(ctx); err != nil {
		t.Fatalf("reconcile consumed root record after grant expiry: %v", err)
	}
	var crashState string
	if err = pool.QueryRow(ctx, `SELECT state FROM artifact_operations WHERE id=$1`, crashGrant.ArtifactID).Scan(&crashState); err != nil || crashState != "consumed" {
		t.Fatalf("crash recovery state=%q err=%v", crashState, err)
	}
	if artifactTransport.consumeCount != consumeCountAfterRoot {
		t.Fatal("recovery re-consumed an already deleted root artifact")
	}
	issued, err = service.Get(ctx, certificate.ID)
	if err != nil {
		t.Fatal(err)
	}
	expiredArtifactID := uuid.Must(uuid.NewV7())
	expiredApprovalID := approvedCertificateAction(t, ctx, service, approvalService, workspaceID, nodeID, certificate.ID, requesterID, requesterSession, approverID, approverSession, "certificate.private_key.export", issued.Version, "certificate_p12", expiredArtifactID)
	expiredGrant, replayed, err := service.CreateP12(ctx, P12Request{CertificateID: certificate.ID, ApprovalID: expiredApprovalID, ArtifactRequestID: expiredArtifactID, CertificateVersion: issued.Version, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-p12-expired", Reason: "expired export", RequestID: "p12-expired-request", Traceparent: "00-3123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if err != nil || replayed {
		t.Fatalf("expired fixture grant=%+v replay=%v err=%v", expiredGrant, replayed, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE artifact_operations SET state='ready',content_sha256=$2,content_size=$3,expires_at=now()-interval '1 second' WHERE id=$1`, expiredGrant.ArtifactID, digest[:], len(artifactBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err = service.OpenArtifact(ctx, expiredGrant.ArtifactID, expiredGrant.DownloadToken, requesterID); !errors.Is(err, ErrArtifactDenied) {
		t.Fatalf("expired artifact err=%v", err)
	}
	if err := service.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	issued, err = service.Get(ctx, certificate.ID)
	if err != nil || issued.State != "expiring" {
		t.Fatalf("maintained certificate=%+v err=%v", issued, err)
	}
	hashArtifactID := uuid.Must(uuid.NewV7())
	hashApprovalID := approvedCertificateAction(t, ctx, service, approvalService, workspaceID, nodeID, certificate.ID, requesterID, requesterSession, approverID, approverSession, "certificate.private_key.export", issued.Version, "certificate_p12", hashArtifactID)
	hashGrant, replayed, err := service.CreateP12(ctx, P12Request{CertificateID: certificate.ID, ApprovalID: hashApprovalID, ArtifactRequestID: hashArtifactID, CertificateVersion: issued.Version, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-p12-hash", Reason: "integrity export", RequestID: "p12-hash-request", Traceparent: "00-4123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if err != nil || replayed {
		t.Fatalf("hash fixture grant=%+v replay=%v err=%v", hashGrant, replayed, err)
	}
	wrongDigest := sha256.Sum256([]byte("different artifact"))
	if _, err = pool.Exec(ctx, `UPDATE artifact_operations SET state='ready',content_sha256=$2,content_size=$3 WHERE id=$1`, hashGrant.ArtifactID, wrongDigest[:], len(artifactBytes)); err != nil {
		t.Fatal(err)
	}
	download, err = service.OpenArtifact(ctx, hashGrant.ArtifactID, hashGrant.DownloadToken, requesterID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, download.Reader)
	_ = download.Reader.Close()
	if err := service.CompleteArtifact(ctx, hashGrant.ArtifactID, download.GrantID, download.Grant, digest[:], int64(len(artifactBytes)), requesterID, requesterSession, "artifact-hash-mismatch"); !errors.Is(err, ErrArtifactDenied) {
		t.Fatalf("artifact hash mismatch err=%v", err)
	}
	if err := service.AbortArtifact(ctx, hashGrant.ArtifactID, download.GrantID); err != nil {
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
	expired, err := service.Get(ctx, certificate.ID)
	if err != nil || expired.State != "expired" {
		t.Fatalf("expired certificate=%+v err=%v", expired, err)
	}
	if _, err := service.OpenArtifact(ctx, hashGrant.ArtifactID, hashGrant.DownloadToken, requesterID); !errors.Is(err, ErrArtifactDenied) {
		t.Fatalf("certificate expiry left artifact downloadable: %v", err)
	}
	var expiringAlerts, expiredAlerts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE kind='certificate.expiring'),count(*) FILTER(WHERE kind='certificate.expired') FROM security_alerts WHERE workspace_id=$1 AND node_id=$2 AND resource_type='certificate' AND resource_id=$3`, workspaceID, nodeID, certificate.ID).Scan(&expiringAlerts, &expiredAlerts); err != nil || expiringAlerts != 1 || expiredAlerts != 1 {
		t.Fatalf("certificate alerts expiring=%d expired=%d err=%v", expiringAlerts, expiredAlerts, err)
	}
	afterExpiryArtifact := uuid.Must(uuid.NewV7())
	afterExpiryApproval := approvedCertificateAction(t, ctx, service, approvalService, workspaceID, nodeID, certificate.ID, requesterID, requesterSession, approverID, approverSession, "certificate.private_key.export", expired.Version, "certificate_p12", afterExpiryArtifact)
	if _, _, err := service.CreateP12(ctx, P12Request{CertificateID: certificate.ID, ApprovalID: afterExpiryApproval, ArtifactRequestID: afterExpiryArtifact, CertificateVersion: expired.Version, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-p12-after-certificate-expiry", Reason: "reject expired certificate", RequestID: "p12-after-certificate-expiry"}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("expired certificate P12 err=%v", err)
	}
	pki.revokeUnavailable = true
	revokeApprovalID := approvedCertificateAction(t, ctx, service, approvalService, workspaceID, nodeID, certificate.ID, requesterID, requesterSession, approverID, approverSession, "certificate.revoke", expired.Version, "retire certificate", uuid.Nil)
	op, replayed, err := service.Revoke(ctx, RevokeRequest{CertificateID: certificate.ID, ApprovalID: revokeApprovalID, CertificateVersion: expired.Version, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-revoke", Reason: "retire certificate", RequestID: "revoke-request", Traceparent: "00-2123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if !errors.Is(err, ErrSignerUnavailable) || replayed || op.ID == "" {
		t.Fatalf("durable revoke=%+v replay=%v err=%v", op, replayed, err)
	}
	var revokeState string
	var held bool
	if err := pool.QueryRow(ctx, `SELECT c.state,o.available_at>now() FROM certificates c JOIN operations p ON p.workspace_id=c.workspace_id AND p.idempotency_key='i17-revoke' JOIN outbox_events o ON o.command_id=p.command_id WHERE c.id=$1`, certificate.ID).Scan(&revokeState, &held); err != nil || revokeState != "revocation_unknown" || !held {
		t.Fatalf("durable revoke intent state=%q held=%v err=%v", revokeState, held, err)
	}
	pki.revokeUnavailable = false
	if _, err := pool.Exec(ctx, `UPDATE commands SET expires_at=created_at+interval '1 microsecond' WHERE operation_id=$1::uuid`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := operations.New(pool).Expire(ctx); err != nil {
		t.Fatal(err)
	}
	op, replayed, err = service.Revoke(ctx, RevokeRequest{CertificateID: certificate.ID, ApprovalID: revokeApprovalID, CertificateVersion: expired.Version, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-revoke", Reason: "retire certificate", RequestID: "revoke-request", Traceparent: "00-2123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if !errors.Is(err, ErrNotReady) || !replayed || pki.revoked {
		t.Fatalf("expired revoke retry=%+v replay=%v external=%v err=%v", op, replayed, pki.revoked, err)
	}
	revocationUnknown, fetchErr := service.Get(ctx, certificate.ID)
	if fetchErr != nil {
		t.Fatal(fetchErr)
	}
	recoveredApprovalID := approvedCertificateAction(t, ctx, service, approvalService, workspaceID, nodeID, certificate.ID, requesterID, requesterSession, approverID, approverSession, "certificate.revoke", revocationUnknown.Version, "retire certificate", uuid.Nil)
	op, replayed, err = service.Revoke(ctx, RevokeRequest{CertificateID: certificate.ID, ApprovalID: recoveredApprovalID, CertificateVersion: revocationUnknown.Version, ActorIdentityID: requesterID, ActorSessionID: requesterSession, ExpectedVersion: 1, IdempotencyKey: "i17-revoke-recovered", Reason: "retire certificate", RequestID: "revoke-recovered", Traceparent: "00-2123456789abcdef0123456789abcdef-0123456789abcdef-01"})
	if err != nil || replayed || op.ID == "" || !pki.revoked {
		t.Fatalf("recovered revoke=%+v replay=%v external=%v err=%v", op, replayed, pki.revoked, err)
	}
	if err := pool.QueryRow(ctx, `SELECT available_at<=now() FROM outbox_events WHERE command_id=(SELECT command_id FROM operations WHERE id=$1::uuid)`, op.ID).Scan(&held); err != nil || !held {
		t.Fatalf("released revoke outbox=%v err=%v", held, err)
	}
}

func approvedCertificateAction(t *testing.T, ctx context.Context, service *Service, approvalService *approvals.Service, workspaceID, nodeID, certificateID, requesterID, requesterSession, approverID, approverSession uuid.UUID, action string, certificateVersion int64, purpose string, artifactID uuid.UUID) uuid.UUID {
	t.Helper()
	boundWorkspace, boundNode, hash, summary, err := service.ActionApprovalBinding(ctx, action, certificateID, certificateVersion, purpose, artifactID)
	if err != nil || boundWorkspace != workspaceID || boundNode != nodeID {
		t.Fatalf("action binding workspace=%s node=%s err=%v", boundWorkspace, boundNode, err)
	}
	approval, err := approvalService.Create(ctx, approvals.Request{WorkspaceID: workspaceID, RequesterID: requesterID, ResourceID: certificateID, Action: action, ResourceType: "certificate", Reason: "review certificate action", TTL: time.Hour, SessionID: requesterSession, RequestID: uuid.Must(uuid.NewV7()).String(), RequestHash: hash, RequestSummary: summary, AuthorityResources: []approvals.AuthorityResource{{WorkspaceID: workspaceID, Type: "node", ID: nodeID}}})
	if err != nil {
		t.Fatal(err)
	}
	approval, err = approvalService.Approve(ctx, approvals.Decision{ApprovalID: approval.ID, ApproverID: approverID, SessionID: approverSession, Reason: "independent review", RequestID: uuid.Must(uuid.NewV7()).String(), ExpectedRequestHash: approval.RequestHash})
	if err != nil {
		t.Fatal(err)
	}
	return approval.ID
}

func cleanupCertificateIntegration(ctx context.Context, pool *pgxpool.Pool, workspaceID uuid.UUID) error {
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only`); err != nil {
		return err
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only`)
	}()
	statements := []string{`DELETE FROM artifact_operations WHERE workspace_id=$1`, `DELETE FROM certificates WHERE workspace_id=$1`, `DELETE FROM secret_provider_refs WHERE workspace_id=$1`, `DELETE FROM agent_command_results WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM node_command_leases WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM command_attempts WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, `DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id=$1)`, `DELETE FROM audit_events WHERE workspace_id=$1`, `DELETE FROM security_alerts WHERE workspace_id=$1`, `DELETE FROM approval_requests WHERE workspace_id=$1`, `DELETE FROM commands WHERE workspace_id=$1`, `DELETE FROM operations WHERE workspace_id=$1`, `DELETE FROM role_bindings WHERE workspace_id=$1`, `DELETE FROM node_sealing_keys WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, `DELETE FROM node_capabilities WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`, `DELETE FROM nodes WHERE workspace_id=$1`, `DELETE FROM identities WHERE issuer='test' AND subject LIKE 'i17-%'`, `DELETE FROM workspaces WHERE id=$1`}
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
