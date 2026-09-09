package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

type artifactTransportFixture struct {
	data     []byte
	consumes int
	fail     bool
}

func (f *artifactTransportFixture) FetchArtifact(context.Context, *agentv1.ArtifactGrantV1, *agentv1.FenceBindingV2) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.data)), nil
}
func (f *artifactTransportFixture) ConsumeArtifact(context.Context, *agentv1.ArtifactGrantV1, []byte, int64, *agentv1.FenceBindingV2) error {
	if f.fail {
		return errors.New("isolated root unavailable")
	}
	f.consumes++
	return nil
}
func (f *artifactTransportFixture) ConfirmArtifactConsumed(context.Context, *agentv1.ArtifactGrantV1, []byte, int64, *agentv1.FenceBindingV2) (bool, error) {
	return f.consumes > 0, nil
}

func TestRealArtifactDownloadTransactions(t *testing.T) {
	owner, _, options := migrateFixture(t)
	ctx := context.Background()
	id := func() uuid.UUID { return uuid.Must(uuid.NewV7()) }
	workspace, node, actor, session, certificate, approval, issueOperation, exportOperation, artifact := id(), id(), id(), id(), id(), id(), id(), id(), id()
	now := time.Now().UTC().Truncate(time.Microsecond)
	later := now.Add(time.Hour)
	stamp, _ := value.FromTime(now)
	expires, _ := value.FromTime(later)
	run := func(sql string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,?,?,?,?)`, UUIDBytes(workspace), "artifact", "artifact-"+workspace.String(), now, now)
	run(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)VALUES(?,?,?,'offline',1,?,?)`, UUIDBytes(node), UUIDBytes(workspace), "node-"+node.String(), now, now)
	run(`INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES(?,?,?,?,?)`, UUIDBytes(actor), "artifact", actor.String(), now, now)
	for _, operation := range []uuid.UUID{issueOperation, exportOperation} {
		run(`INSERT INTO operations(id,workspace_id,node_id,state,request_id,created_at,updated_at)VALUES(?,?,?,'succeeded',?,?,?)`, UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), operation.String(), now, now)
	}
	run(`INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,expires_at,created_at)VALUES(?,?,?,'certificate.p12','certificate',?,'test','pending',?,?)`, UUIDBytes(approval), UUIDBytes(workspace), UUIDBytes(actor), UUIDBytes(certificate), expires, stamp)
	// This fixture starts at a previously issued, explicitly legacy certificate;
	// issuance/attestation is exercised by the PostgreSQL lifecycle suite.
	run(`INSERT INTO certificates(id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,version,csr_receipt_legacy,not_after,created_at,updated_at)VALUES(?,?,?,?,?,'[]',2048,'issued',1,1,?,?,?)`, UUIDBytes(certificate), UUIDBytes(workspace), UUIDBytes(node), UUIDBytes(issueOperation), "artifact.example.test", later, now, now)
	data := []byte("isolated encrypted artifact")
	digest := sha256.Sum256(data)
	token := strings.Repeat("a", 43)
	tokenHash := sha256.Sum256([]byte(token))
	run(`INSERT INTO artifact_operations(id,workspace_id,node_id,certificate_id,operation_id,purpose,state,content_sha256,content_size,token_sha256,request_hash,expires_at,created_at,updated_at,approval_id,certificate_version)VALUES(?,?,?,?,?,'certificate_p12','ready',?,?,?,?,?,?,?,?,1)`, UUIDBytes(artifact), UUIDBytes(workspace), UUIDBytes(node), UUIDBytes(certificate), UUIDBytes(exportOperation), digest[:], len(data), tokenHash[:], tokenHash[:], later, now, now, UUIDBytes(approval))
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
	transport := &artifactTransportFixture{data: data}
	var seed [32]byte
	seed[0] = 12
	service := certificates.NewArtifactDownloads(backend, transport, commandauth.NewSignerFromSeed(seed))
	if _, err := service.OpenArtifact(ctx, artifact, token, id()); !errors.Is(err, certificates.ErrArtifactDenied) {
		t.Fatal("requester mismatch", err)
	}
	if _, err := service.OpenArtifact(ctx, artifact, strings.Repeat("b", 43), actor); !errors.Is(err, certificates.ErrArtifactDenied) {
		t.Fatal("token mismatch", err)
	}
	download, err := service.OpenArtifact(ctx, artifact, token, actor)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(download.Reader)
	download.Reader.Close()
	if err != nil || !bytes.Equal(contents, data) {
		t.Fatal("download bytes", err)
	}
	if err := service.AbortArtifact(ctx, artifact, download.GrantID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.OpenArtifact(ctx, artifact, token, actor); !errors.Is(err, certificates.ErrArtifactDenied) {
		t.Fatal("abort released live grant", err)
	}
	if err := service.CompleteArtifact(ctx, artifact, download.GrantID, download.Grant, digest[:], int64(len(data)), actor, session, "download-1"); err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteArtifact(ctx, artifact, download.GrantID, download.Grant, digest[:], int64(len(data)), actor, session, "download-1"); err != nil {
		t.Fatal("exact replay", err)
	}
	if transport.consumes != 1 {
		t.Fatal("replay repeated root mutation", transport.consumes)
	}
	if err := service.CompleteArtifact(ctx, artifact, download.GrantID, download.Grant, digest[:], int64(len(data)), actor, session, "download-1 "); !errors.Is(err, certificates.ErrArtifactDenied) {
		t.Fatal("trailing-space replay accepted", err)
	}
	if _, err := service.OpenArtifact(ctx, artifact, token, actor); !errors.Is(err, certificates.ErrArtifactDenied) {
		t.Fatal("consumed artifact reopened", err)
	}
	verification, err := audit.NewBackendManager(backend, bytes.Repeat([]byte{7}, 32)).Verify(ctx, workspace)
	if err != nil || !verification.Valid {
		t.Fatal("download audit", verification, err)
	}
	var count int
	if err := owner.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE workspace_id=? AND action='certificate.p12.download'`, UUIDBytes(workspace)).Scan(&count); err != nil || count != 1 {
		t.Fatal("audit exactly once", count, err)
	}
}
