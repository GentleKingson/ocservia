package mysql

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

// TestRealVersionFiveDataUpgrade populates the v5 representation with native
// DATETIME attestation rows, upgrades through the appended version 6, and
// proves the published receipts and every finite historical value — including
// NULL validity and consumption — are preserved exactly.
func TestRealVersionFiveDataUpgrade(t *testing.T) {
	b, _ := versionTwoFixture(t, false)
	ctx := context.Background()
	chain, err := loadRevisionChain(b.engine)
	if err != nil {
		t.Fatal(err)
	}
	conn, lock, err := migrationConnection(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	err = b.migrateChainOn(ctx, conn, chain[:4], "")
	unlock := releaseMigrationConnection(conn, lock)
	if err != nil || unlock != nil {
		t.Fatal(err, unlock)
	}
	receipts := func() []string {
		t.Helper()
		var result []string
		for _, query := range []string{
			`SELECT CONCAT_WS('|',version,parent_checksum,manifest_checksum,state,repair_count,started_at,verified_at) FROM backend_schema_revisions WHERE version<=5 ORDER BY version`,
			`SELECT CONCAT_WS('|',version,ordinal,name,checksum,state,started_at,verified_at) FROM backend_schema_revision_steps WHERE version<=5 ORDER BY version,ordinal`,
		} {
			rows, err := b.Query(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var row string
				if err = rows.Scan(&row); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				result = append(result, row)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	before := receipts()
	workspace, node, identity, session := uuid.New(), uuid.New(), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	base := time.Date(2026, 9, 9, 12, 30, 5, 123456000, time.UTC)
	if _, err = b.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,?,?,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`, UUIDBytes(workspace), "v6-history", "v6-"+workspace.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'v6-node','active',?,?)`, UUIDBytes(node), UUIDBytes(workspace), base, base); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES(?, 'v6-history','v6',?,?)`, UUIDBytes(identity), fixtureTimestamp(t, base), fixtureTimestamp(t, base)); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES(?,?,?,?)`, UUIDBytes(session), UUIDBytes(identity), fixtureTimestamp(t, base.Add(time.Hour)), fixtureTimestamp(t, base)); err != nil {
		t.Fatal(err)
	}
	consumed, pending := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	secret, other := make([]byte, 32), make([]byte, 32)
	for i := range secret {
		secret[i], other[i] = byte(i+1), byte(i+2)
	}
	nonce, context := make([]byte, 32), make([]byte, 32)
	if _, err = b.Exec(ctx, `INSERT INTO privd_attestation_enrollment_credentials(id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,consumed_at,created_by_identity_id,created_by_session_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		UUIDBytes(consumed), UUIDBytes(node), secret, nonce, context, base.Add(30*time.Minute), base.Add(2*time.Minute), UUIDBytes(identity), UUIDBytes(session), base.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `INSERT INTO privd_attestation_enrollment_credentials(id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,created_by_identity_id,created_by_session_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		UUIDBytes(pending), UUIDBytes(node), other, nonce, context, base.Add(30*time.Minute), UUIDBytes(identity), UUIDBytes(session), base.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	activeKey, revokedKey := "ed25519-sha256:"+strings.Repeat("ab", 32), "ed25519-sha256:"+strings.Repeat("cd", 32)
	if _, err = b.Exec(ctx, `INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,valid_until,predecessor_key_id,registration_credential_id) VALUES(?,?,'ed25519',?,'active',?,?,?,?,NULL,?)`,
		UUIDBytes(node), activeKey, secret, base, base, base, base.Add(24*time.Hour), UUIDBytes(consumed)); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec(ctx, `INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,valid_until,revoked_at,registration_credential_id) VALUES(?,?,'ed25519',?,'revoked',?,?,?,?,?,?)`,
		UUIDBytes(node), revokedKey, other, base, base, base, base.Add(time.Hour), base.Add(time.Hour), UUIDBytes(pending)); err != nil {
		t.Fatal(err)
	}
	if err = b.Migrate(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, receipts()) {
		t.Fatal("published v5 history rewritten")
	}
	wantBase, wantConsumed := fixtureTimestamp(t, base.Add(-time.Minute)), fixtureTimestamp(t, base)
	var expires, consumedAt, createdAt value.Timestamp
	if err = b.QueryRow(ctx, `SELECT expires_at,consumed_at,created_at FROM privd_attestation_enrollment_credentials WHERE id=?`, UUIDBytes(consumed)).Scan(&expires, &consumedAt, &createdAt); err != nil {
		t.Fatal(err)
	}
	if expires != fixtureTimestamp(t, base.Add(30*time.Minute)) || consumedAt != fixtureTimestamp(t, base.Add(2*time.Minute)) || createdAt != wantBase {
		t.Fatalf("credential times changed: %v %v %v", expires, consumedAt, createdAt)
	}
	if err = b.QueryRow(ctx, `SELECT expires_at,consumed_at FROM privd_attestation_enrollment_credentials WHERE id=?`, UUIDBytes(pending)).Scan(&expires, &consumedAt); err != nil {
		t.Fatal(err)
	}
	if expires != fixtureTimestamp(t, base.Add(30*time.Minute)) || consumedAt.Valid {
		t.Fatalf("pending credential times: %v %v", expires, consumedAt)
	}
	var created, approved, activated, until, revoked value.Timestamp
	if err = b.QueryRow(ctx, `SELECT created_at,approved_at,activated_at,valid_until,revoked_at FROM node_privd_attestation_keys WHERE key_id=?`, activeKey).Scan(&created, &approved, &activated, &until, &revoked); err != nil {
		t.Fatal(err)
	}
	if created != wantConsumed || approved != wantConsumed || activated != wantConsumed || until != fixtureTimestamp(t, base.Add(24*time.Hour)) || revoked.Valid {
		t.Fatalf("active key times changed: %v %v %v %v %v", created, approved, activated, until, revoked)
	}
	if err = b.QueryRow(ctx, `SELECT activated_at,valid_until,revoked_at FROM node_privd_attestation_keys WHERE key_id=?`, revokedKey).Scan(&activated, &until, &revoked); err != nil {
		t.Fatal(err)
	}
	if activated != wantConsumed || until != fixtureTimestamp(t, base.Add(time.Hour)) || revoked != until {
		t.Fatalf("revoked key times changed: %v %v %v", activated, until, revoked)
	}
	if err = b.ValidateSchema(ctx, 34); err != nil {
		t.Fatal(err)
	}
}
