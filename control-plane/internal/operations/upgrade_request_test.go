package operations

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/google/uuid"
)

type upgradePreparationBackend struct {
	database.Backend
	store *upgradePreparationStore
}

func (b upgradePreparationBackend) Begin(context.Context, database.Isolation) (database.Tx, error) {
	return upgradePreparationTx{store: b.store}, nil
}

type upgradePreparationTx struct {
	database.Tx
	store *upgradePreparationStore
}

func (t upgradePreparationTx) OperationStore() operationstore.Store { return t.store }
func (upgradePreparationTx) Commit(context.Context) error           { return nil }
func (upgradePreparationTx) Rollback(context.Context) error         { return nil }

type upgradePreparationStore struct {
	operationstore.Store
	workspace             uuid.UUID
	architecture, version string
	err                   error
	ctx                   context.Context
	node                  uuid.UUID
	reads                 int
}

func (s *upgradePreparationStore) UpgradeNode(ctx context.Context, node uuid.UUID) (uuid.UUID, string, string, error) {
	s.ctx, s.node = ctx, node
	s.reads++
	if s.err != nil {
		return uuid.Nil, "", "", s.err
	}
	return s.workspace, s.architecture, "debian", nil
}

func upgradePreparationCatalog(t *testing.T) *releasecatalog.Catalog {
	t.Helper()
	path := filepath.Join(t.TempDir(), "releases.json")
	if err := os.WriteFile(path, []byte(`{"releases":[{"version":"2.0.0","architecture":"amd64","package_format":"deb","package_sha256":"`+strings.Repeat("43", 32)+`"},{"version":"2.0.0","architecture":"arm64","package_format":"deb","package_sha256":"`+strings.Repeat("44", 32)+`"},{"version":"1.0.0","architecture":"amd64","package_format":"deb","package_sha256":"`+strings.Repeat("45", 32)+`"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := releasecatalog.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestAgentUpgradePreparation(t *testing.T) {
	workspace, node := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	catalog := upgradePreparationCatalog(t)
	for _, tc := range []struct {
		name, target, architecture, observed string
		foreign, noCatalog                   bool
		readErr, prepareErr                  error
	}{
		{name: "amd64", target: "2.0.0", architecture: "amd64", observed: "1.2.0"},
		{name: "arm64-and-trim", target: " 2.0.0 ", architecture: "arm64", observed: "1.2.0"},
		{name: "invalid-before-read", target: "https://untrusted/release", readErr: database.ErrNotFound, prepareErr: ErrInvalidRequest},
		{name: "missing-node", target: "2.0.0", readErr: database.ErrNotFound, prepareErr: ErrNodeUnavailable},
		{name: "read-failure", target: "2.0.0", readErr: database.ErrPermission, prepareErr: ErrNodeUnavailable},
		{name: "canceled-read", target: "2.0.0", readErr: context.Canceled, prepareErr: ErrNodeUnavailable},
		{name: "workspace-before-architecture", target: "2.0.0", foreign: true, prepareErr: ErrNodeUnavailable},
		{name: "architecture-before-catalog", target: "3.0.0", observed: "unknown", prepareErr: ErrUpgradeArchitectureUnknown},
		{name: "unsupported-architecture", target: "2.0.0", architecture: "riscv64", observed: "1.0.0", prepareErr: ErrUpgradeReleaseNotTrusted},
		{name: "catalog-before-version", target: "3.0.0", architecture: "amd64", prepareErr: ErrUpgradeReleaseNotTrusted},
		{name: "no-catalog", target: "2.0.0", architecture: "amd64", noCatalog: true, prepareErr: ErrUpgradeReleaseNotTrusted},
		{name: "unobserved-version", target: "2.0.0", architecture: "amd64"},
		{name: "unknown-version", target: "2.0.0", architecture: "amd64", observed: "unknown"},
		{name: "same-version", target: "2.0.0", architecture: "amd64", observed: "2.0.0"},
		{name: "downgrade", target: "1.0.0", architecture: "amd64", observed: "1.2.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &upgradePreparationStore{workspace: workspace, architecture: tc.architecture, version: tc.observed, err: tc.readErr}
			if tc.foreign {
				store.workspace = uuid.Must(uuid.NewV7())
			}
			service := NewBackend(upgradePreparationBackend{store: store}, 50, nil)
			if !tc.noCatalog {
				service.EnableReleaseCatalog(catalog)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			target, err := service.PrepareAgentUpgrade(ctx, workspace, node, tc.target)
			if !errors.Is(err, tc.prepareErr) {
				t.Fatalf("prepare: %v want %v", err, tc.prepareErr)
			}
			if tc.prepareErr != nil && target != (AgentUpgradeTarget{}) {
				t.Fatal("failed preparation exposed a target")
			}
			if tc.readErr != nil && tc.prepareErr != ErrInvalidRequest && !errors.Is(err, tc.readErr) {
				t.Fatalf("lost read cause: %v", err)
			}
			if tc.prepareErr == nil {
				if target.Version != strings.TrimSpace(tc.target) || target.Architecture != tc.architecture {
					t.Fatalf("target: %+v", target)
				}
				target.PackageSHA256[0] ^= 0xff
				again, err := service.PrepareAgentUpgrade(ctx, workspace, node, tc.target)
				if err != nil || target.PackageSHA256 == again.PackageSHA256 {
					t.Fatal("target aliases catalog state")
				}
			}
			if tc.prepareErr == ErrInvalidRequest {
				if store.reads != 0 {
					t.Fatal("invalid target read the backend")
				}
			} else if store.ctx != ctx || store.node != node {
				t.Fatal("preparation changed the request context or node")
			}
		})
	}
	t.Run("unsupported-store", func(t *testing.T) {
		_, err := NewBackend(nil, 50, nil).PrepareAgentUpgrade(t.Context(), workspace, node, "2.0.0")
		if !errors.Is(err, ErrNodeUnavailable) {
			t.Fatal(err)
		}
	})
}
