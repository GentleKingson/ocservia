package enrollment

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/google/uuid"
)

// The integration runner owns the disposable server and restarts it only after
// ready is written. Ordinary package tests cannot restart a shared database.
func TestNodeLifecycleRestartBackendIntegration(t *testing.T) {
	gate := os.Getenv("OCSERV_TEST_RESTART_GATE")
	if gate == "" {
		t.Skip("disposable database restart runner required")
	}
	f, ctx := newLifecycleFixture(t), context.Background()
	signer, err := commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	s := NewBackend(f.b, "", "test", signer)
	endpoint := endpointFixture(216)
	token := createToken(t, s, f.workspace, endpoint)
	request := enrollmentRequest(token.Value, endpoint)
	response, err := s.Enroll(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	node, err := uuid.FromBytes(response.NodeId)
	if err != nil {
		t.Fatal(err)
	}
	var keys []enrollmentstore.SealingKey
	f.within(t, func(tx database.Tx) error {
		enroll, err := enrollmentstore.Enrollment(tx)
		if err != nil {
			return err
		}
		keys, err = enroll.SealingKeys(ctx, node)
		if err != nil {
			return err
		}
		at, err := database.WallTime(ctx, tx)
		if err != nil {
			return err
		}
		revision, err := enroll.Revoke(ctx, node, at)
		if err != nil {
			return err
		}
		trust, err := enrollmentstore.Trust(tx)
		if err != nil {
			return err
		}
		return trust.Enqueue(ctx, enrollmentstore.TrustJob{NodeID: node, EndpointID: endpoint, DesiredState: "revoked", Revision: revision, Reason: "restart"}, value.Timestamp{Valid: true, Micros: value.NegativeInfinity})
	})
	workerID := uuid.New()
	f.trust(t, func(s enrollmentstore.TrustStore) error {
		job, err := s.Claim(ctx, workerID)
		if err != nil {
			return err
		}
		if job.NodeID != node {
			return errors.New("claimed another restart fixture")
		}
		changed, err := s.MarkUpdateApplied(ctx, job, workerID)
		if err == nil && !changed {
			return errors.New("update not marked")
		}
		return err
	})
	f.exec(t, `UPDATE node_trust_convergence SET locked_until=$1 WHERE node_id=$2`, `UPDATE node_trust_convergence SET locked_until=? WHERE node_id=?`, value.Timestamp{Valid: true, Micros: value.NegativeInfinity}, node)
	before := f.snapshot(t, node)
	if err := os.WriteFile(filepath.Join(gate, "ready"), []byte("committed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(gate, "restarted")); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("database restart was not acknowledged")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Reconnect stale pool sockets after the actual server restart. Only this
	// read-only readiness probe retries; no business transaction is replayed.
	for {
		probe, cancel := context.WithTimeout(ctx, time.Second)
		var one int
		err := f.b.QueryRow(probe, `SELECT 1`).Scan(&one)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if after := f.snapshot(t, node); !reflect.DeepEqual(before, after) {
		t.Fatal("restart changed trust state", before, after)
	}
	s = NewBackend(f.b, "", "test", signer)
	if _, err := s.Enroll(ctx, request); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("restart forgot consumed token", err)
	}
	f.within(t, func(tx database.Tx) error {
		store, err := enrollmentstore.Enrollment(tx)
		if err != nil {
			return err
		}
		got, err := store.SealingKeys(ctx, node)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(keys, got) {
			return errors.New("restart changed sealing keys")
		}
		n, err := store.NodeByID(ctx, node, enrollmentstore.Unlocked)
		if err != nil {
			return err
		}
		if n.Status != "revoked" || n.WorkspaceID != f.workspace {
			return errors.New("restart lost node revocation or scope")
		}
		return nil
	})
	transport := &leasedTransport{update: func(context.Context) error { return errors.New("replayed completed trust update") }}
	worker, err := NewTrustConvergenceWorkerBackend(f.b, transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.RunOnce(ctx); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if after := f.snapshot(t, node); !after.Close || after.Worker != nil || after.Attempts != before.Attempts+1 || transport.closes != 1 {
		t.Fatal("restart did not resume close-only retry", after)
	}
}
