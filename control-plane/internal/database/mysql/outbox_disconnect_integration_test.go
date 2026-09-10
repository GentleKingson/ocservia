package mysql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func TestRealOutboxCommitDisconnect(t *testing.T) {
	for _, phase := range []string{"claim", "sent"} {
		for _, forward := range []bool{false, true} {
			name := phase + "-request-lost"
			if forward {
				name = phase + "-response-lost"
			}
			t.Run(name, func(t *testing.T) {
				owner, _, options := migrateFixture(t)
				ctx := context.Background()
				if err := owner.GrantTestPrivileges(ctx); err != nil {
					t.Fatal(err)
				}
				workspace, node := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
				at := fixtureTimestamp(t, time.Now().UTC())
				if _, err := owner.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'PR04',?,?,?)`, UUIDBytes(workspace), workspace.String(), at, at); err != nil {
					t.Fatal(err)
				}
				if _, err := owner.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'PR04','active',?,?)`, UUIDBytes(node), UUIDBytes(workspace), at, at); err != nil {
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
				signer, err := commandauth.NewRandomSigner()
				if err != nil {
					t.Fatal(err)
				}
				service := operations.NewBackend(runtime, 1, signer)
				if _, _, err := service.CreateSynthetic(ctx, operations.CreateRequest{NodeID: node, IdempotencyKey: uuid.NewString(), ExpectedVersion: 1, Kind: operations.SyntheticEcho, Message: "PR04", TTL: time.Minute, RequestID: uuid.NewString(), Traceparent: "00-" + strings.Repeat("1", 32) + "-" + strings.Repeat("2", 16) + "-01"}); err != nil {
					t.Fatal(err)
				}
				var dispatch operations.Dispatch
				if phase == "sent" {
					jobs, err := service.Claim(ctx, uuid.Must(uuid.NewV7()), 1, time.Minute)
					if err != nil || len(jobs) != 1 {
						t.Fatal(jobs, err)
					}
					dispatch = jobs[0]
				}
				proxy := newFinalizeProxy(t, cfg.Addr, "COMMIT", forward)
				cfg.Addr = proxy.listener.Addr().String()
				options.DSN = cfg.FormatDSN()
				proxied, err := Open(ctx, options)
				if err != nil {
					t.Fatal(err)
				}
				defer proxied.Close()
				service = operations.NewBackend(proxied, 1, signer)
				bounded, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
				defer cancel()
				if phase == "claim" {
					jobs, claimErr := service.Claim(bounded, uuid.Must(uuid.NewV7()), 1, time.Minute)
					err = claimErr
					if (len(jobs) == 1) != forward {
						t.Fatal("unconfirmed claim escaped", jobs, err)
					}
				} else {
					err = service.MarkSent(bounded, dispatch)
				}
				if (err == nil) != forward {
					t.Fatal("incorrect commit readback", err)
				}
				select {
				case <-proxy.hit:
				default:
					t.Fatal("did not interrupt real commit packet")
				}
				select {
				case <-proxy.disconnected:
				case <-time.After(time.Second):
					t.Fatal("uncertain connection was retained")
				}
				var attempts, sent int
				if err := owner.QueryRow(ctx, `SELECT count(*),count(CASE WHEN state='sent' THEN 1 END) FROM command_attempts`).Scan(&attempts, &sent); err != nil {
					t.Fatal(err)
				}
				wantAttempts, wantSent := 1, 0
				if phase == "claim" && !forward {
					wantAttempts = 0
				}
				if phase == "sent" && forward {
					wantSent = 1
				}
				if attempts != wantAttempts || sent != wantSent {
					t.Fatal("commit was blindly replayed or partially persisted", attempts, sent)
				}
			})
		}
	}
}
