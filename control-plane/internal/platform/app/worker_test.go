package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
)

type stoppedBackend struct{ database.Backend }

func (stoppedBackend) Begin(ctx context.Context, _ database.Isolation) (database.Tx, error) {
	return nil, ctx.Err()
}

func socketDirectory(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, ".app-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestWorkerRoleAssembly(t *testing.T) {
	signer, err := commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []config.Role{config.RoleAPI, config.RoleWorker, config.RoleScheduler, config.RoleAll} {
		for _, mode := range []string{"bare", "simulator", "endpoint", "endpoint-and-simulator"} {
			t.Run(string(role)+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				life := newLifecycle(ctx, time.Second, quietLogger())
				cfg := config.Config{Role: role, TransportSocket: "/unused.sock", TransportTimeout: time.Second, TransportQueue: 8, OwnerLeaseTTL: 30 * time.Second, TrustSocket: filepath.Join(socketDirectory(t), "trust.sock")}
				cfg.LocalSimulator = strings.Contains(mode, "simulator")
				if strings.Contains(mode, "endpoint") {
					cfg.ControllerEndpointID = strings.Repeat("ab", 32)
				}
				backend := stoppedBackend{}
				service, fence, err := startWorker(life, cfg, BuildInfo{}, backend, signer, operationstore.NewBackend(backend, 50, signer), quietLogger())
				if err != nil {
					_ = life.close(err)
					t.Fatal(err)
				}
				if service == nil {
					t.Fatal("shared local slice service missing")
				}
				want := 0
				workerRole := role == config.RoleWorker || role == config.RoleAll
				if workerRole && mode != "bare" {
					want = 3
				}
				owner := workerRole && strings.Contains(mode, "endpoint")
				if owner {
					want += 3
				}
				_, hasOwner := fence.(*ownersession.Manager)
				if len(life.tasks) != want || hasOwner != owner || (life.trust != nil) != owner {
					t.Fatalf("tasks=%d owner=%v trust=%v", len(life.tasks), hasOwner, life.trust != nil)
				}
				if err := life.close(ctx.Err()); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if _, err := os.Stat(cfg.TrustSocket); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("Trust socket survived cleanup", err)
				}
			})
		}
	}
}

func TestWorkerPartialStartupClosesTrust(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	life := newLifecycle(ctx, time.Second, quietLogger())
	cfg := config.Config{Role: config.RoleWorker, ControllerEndpointID: strings.Repeat("ab", 32), TransportSocket: "/unused.sock", TransportTimeout: time.Second, TransportQueue: 8, OwnerLeaseTTL: 30 * time.Second, TrustSocket: filepath.Join(socketDirectory(t), "trust.sock"), TestResultCommitBarrier: "/missing/pr01-barrier"}
	signer, err := commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = startWorker(life, cfg, BuildInfo{}, stoppedBackend{}, signer, nil, quietLogger())
	if err == nil || !strings.Contains(err.Error(), "configure result commit barrier") {
		t.Fatalf("expected later construction failure: %v", err)
	}
	if life.trust == nil || len(life.tasks) != 3 {
		t.Fatal("fixture did not start Owner and Trust before failing")
	}
	if _, statErr := os.Stat(cfg.TrustSocket); statErr != nil {
		t.Fatal("fixture did not bind Trust socket", statErr)
	}
	if result := life.close(err); !errors.Is(result, err) {
		t.Fatal("startup failure replaced", result)
	}
	if _, statErr := os.Stat(cfg.TrustSocket); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("partial startup leaked Trust socket", statErr)
	}
}
