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
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type artifactTransportFixture struct {
	data     []byte
	consumes int
	fail     bool
}

type artifactRejectedFence struct {
	coordination.Fence
	err error
}

func (f artifactRejectedFence) AssertTransaction(context.Context, database.Tx) error { return f.err }

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
	run(`INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,?,?,?,?)`, UUIDBytes(workspace), "artifact", "artifact-"+workspace.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	run(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at)VALUES(?,?,?,'offline',1,?,?)`, UUIDBytes(node), UUIDBytes(workspace), "node-"+node.String(), fixtureTimestamp(t, now), fixtureTimestamp(t, now))
	run(`INSERT INTO identities(id,issuer,subject,created_at,updated_at)VALUES(?,?,?,?,?)`, UUIDBytes(actor), "artifact", actor.String(), stamp, stamp)
	for _, operation := range []uuid.UUID{issueOperation, exportOperation} {
		run(`INSERT INTO operations(id,workspace_id,node_id,state,request_id,created_at,updated_at)VALUES(?,?,?,'succeeded',?,?,?)`, UUIDBytes(operation), UUIDBytes(workspace), UUIDBytes(node), operation.String(), stamp, stamp)
	}
	run(`INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,expires_at,created_at)VALUES(?,?,?,'certificate.p12','certificate',?,'test','pending',?,?)`, UUIDBytes(approval), UUIDBytes(workspace), UUIDBytes(actor), UUIDBytes(certificate), expires, stamp)
	// This fixture starts at a previously issued, explicitly legacy certificate;
	// issuance/attestation is exercised by the PostgreSQL lifecycle suite.
	run(`INSERT INTO certificates(id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,version,csr_receipt_legacy,not_after,created_at,updated_at)VALUES(?,?,?,?,?,'[]',2048,'issued',1,1,?,?,?)`, UUIDBytes(certificate), UUIDBytes(workspace), UUIDBytes(node), UUIDBytes(issueOperation), "artifact.example.test", expires, stamp, stamp)
	data := []byte("isolated encrypted artifact")
	digest := sha256.Sum256(data)
	token := strings.Repeat("a", 43)
	tokenHash := sha256.Sum256([]byte(token))
	run(`INSERT INTO artifact_operations(id,workspace_id,node_id,certificate_id,operation_id,purpose,state,content_sha256,content_size,token_sha256,request_hash,expires_at,created_at,updated_at,approval_id,certificate_version)VALUES(?,?,?,?,?,'certificate_p12','ready',?,?,?,?,?,?,?,?,1)`, UUIDBytes(artifact), UUIDBytes(workspace), UUIDBytes(node), UUIDBytes(certificate), UUIDBytes(exportOperation), digest[:], len(data), tokenHash[:], tokenHash[:], expires, stamp, stamp, UUIDBytes(approval))
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
	for _, at := range []value.Timestamp{{Valid: true, Micros: value.NegativeInfinity}, {Valid: true, Micros: value.PositiveInfinity}, {Valid: true, Micros: value.MinTimestamp}, {Valid: true, Micros: value.EndTimestamp - 1}} {
		names, err := value.ParseJSONB([]byte(`["example.test",null,[1.2345678901234567890123456789],{"nested":true}]`))
		if err != nil {
			t.Fatal(err)
		}
		run(`UPDATE certificates SET dns_names=?,not_before=?,not_after=?,revoked_at=?,created_at=?,updated_at=? WHERE id=?`, names, at, at, at, at, at, UUIDBytes(certificate))
		got, err := service.Get(ctx, certificate)
		if err != nil || got.NotBefore == nil || *got.NotBefore != at || got.NotAfter == nil || *got.NotAfter != at || got.RevokedAt == nil || *got.RevokedAt != at || got.CreatedAt != at || got.UpdatedAt != at || !bytes.Equal(got.DNSNames, names.Bytes()) {
			t.Fatal("logical certificate read", got, err)
		}
		list, err := service.ListNode(ctx, node)
		if err != nil || len(list) != 1 || list[0].CreatedAt != at {
			t.Fatal("certificate list", list, err)
		}
		byOperation, replay, err := service.GetByOperation(ctx, issueOperation, true)
		if err != nil || !replay || byOperation.ID != certificate || byOperation.UpdatedAt != at {
			t.Fatal("certificate operation read", byOperation, replay, err)
		}
	}
	run(`UPDATE certificates SET dns_names='[]',not_before=NULL,not_after=?,revoked_at=NULL,created_at=?,updated_at=? WHERE id=?`, expires, stamp, stamp, UUIDBytes(certificate))
	if _, err := service.Get(ctx, id()); !errors.Is(err, database.ErrNotFound) {
		t.Fatal("missing certificate", err)
	}
	for _, tc := range []struct {
		name     string
		deadline value.Timestamp
		allowed  bool
	}{
		{"null", value.Timestamp{}, false},
		{"negative_infinity", value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, false},
		{"minimum_finite", value.Timestamp{Valid: true, Micros: value.MinTimestamp}, false},
		{"positive_infinity", value.Timestamp{Valid: true, Micros: value.PositiveInfinity}, true},
		{"maximum_finite", value.Timestamp{Valid: true, Micros: value.EndTimestamp - 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run(`UPDATE certificates SET not_after=? WHERE id=?`, tc.deadline, UUIDBytes(certificate))
			artifactDeadline := tc.deadline
			if !artifactDeadline.Valid {
				artifactDeadline = expires
			}
			run(`UPDATE artifact_operations SET expires_at=? WHERE id=?`, artifactDeadline, UUIDBytes(artifact))
			download, err := service.OpenArtifact(ctx, artifact, token, actor)
			if !tc.allowed {
				if !errors.Is(err, certificates.ErrArtifactDenied) {
					t.Fatal("expired or NULL certificate accepted", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			download.Reader.Close()
			var lease, granted value.Timestamp
			if err := owner.QueryRow(ctx, `SELECT lease_until,active_grant_expires_at FROM artifact_operations WHERE id=?`, UUIDBytes(artifact)).Scan(&lease, &granted); err != nil {
				t.Fatal(err)
			}
			if lease != granted || !lease.Valid || lease.Micros <= stamp.Micros || lease.Micros >= stamp.Micros+int64(2*time.Minute/time.Microsecond) {
				t.Fatal("unbounded grant deadline", lease, granted)
			}
			run(`UPDATE artifact_operations SET state='ready',lease_until=NULL,active_grant_id=NULL,active_grant_subject=NULL,active_grant_expires_at=NULL WHERE id=?`, UUIDBytes(artifact))
		})
	}
	run(`UPDATE certificates SET not_after=? WHERE id=?`, expires, UUIDBytes(certificate))
	run(`UPDATE artifact_operations SET expires_at=? WHERE id=?`, expires, UUIDBytes(artifact))
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
	// Recovery confirms root evidence before resetting an expired grant. The
	// durable artifact deadline need not fit time.Time or a protobuf timestamp.
	transport.consumes = 0
	download.Grant.ExpiresAt = timestamppb.New(now.Add(-time.Minute))
	grantBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(download.Grant)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		deadline value.Timestamp
		state    string
	}{
		{"positive_infinity", value.Timestamp{Valid: true, Micros: value.PositiveInfinity}, "ready"},
		{"negative_infinity", value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, "expired"},
	} {
		t.Run("recover_"+tc.name, func(t *testing.T) {
			run(`UPDATE certificates SET state='issued',not_after=? WHERE id=?`, value.Timestamp{Valid: true, Micros: value.PositiveInfinity}, UUIDBytes(certificate))
			run(`UPDATE artifact_operations SET state='consuming',expires_at=?,active_grant_id=?,consume_grant=?,consume_actor_id=?,consume_sha256=?,consume_size=?,consume_session_id=?,consume_request_id='recovery-test' WHERE id=?`, tc.deadline, UUIDBytes(download.GrantID), grantBytes, UUIDBytes(actor), digest[:], len(data), UUIDBytes(session), UUIDBytes(artifact))
			if err := service.Maintain(ctx); err != nil {
				t.Fatal(err)
			}
			var state string
			var active []byte
			if err := owner.QueryRow(ctx, `SELECT state,active_grant_id FROM artifact_operations WHERE id=?`, UUIDBytes(artifact)).Scan(&state, &active); err != nil || state != tc.state || active != nil {
				t.Fatal("recovery", state, active, err)
			}
		})
	}
	deniedFence := errors.New("isolated maintenance fence rejected")
	run(`UPDATE certificates SET state='issued',not_after=? WHERE id=?`, value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, UUIDBytes(certificate))
	if err := service.Maintain(coordination.WithFence(ctx, artifactRejectedFence{err: deniedFence})); !errors.Is(err, deniedFence) {
		t.Fatal("maintenance fence", err)
	}
	var fencedState string
	if err := owner.QueryRow(ctx, `SELECT state FROM certificates WHERE id=?`, UUIDBytes(certificate)).Scan(&fencedState); err != nil || fencedState != "issued" {
		t.Fatal("fence committed certificate", fencedState, err)
	}
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM security_alerts WHERE resource_id=?`, UUIDBytes(certificate)).Scan(&count); err != nil || count != 0 {
		t.Fatal("fence committed alert", count, err)
	}
	for _, tc := range []struct {
		name     string
		deadline value.Timestamp
		state    string
		alerts   int
	}{
		{"positive_infinity", value.Timestamp{Valid: true, Micros: value.PositiveInfinity}, "issued", 0},
		{"null", value.Timestamp{}, "issued", 0},
		{"expiring", expires, "expiring", 1},
		{"negative_infinity", value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, "expired", 2},
	} {
		t.Run("maintain_"+tc.name, func(t *testing.T) {
			run(`UPDATE certificates SET state='issued',not_after=? WHERE id=?`, tc.deadline, UUIDBytes(certificate))
			for range 2 {
				if err := service.Maintain(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var state string
			if err := owner.QueryRow(ctx, `SELECT state FROM certificates WHERE id=?`, UUIDBytes(certificate)).Scan(&state); err != nil || state != tc.state {
				t.Fatal("certificate state", state, err)
			}
			if err := owner.QueryRow(ctx, `SELECT count(*) FROM security_alerts WHERE resource_id=?`, UUIDBytes(certificate)).Scan(&count); err != nil || count != tc.alerts {
				t.Fatal("transition alerts", count, err)
			}
		})
	}
}
