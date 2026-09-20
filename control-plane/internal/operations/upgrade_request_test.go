package operations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac/rbacstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/google/uuid"
)

type upgradePreparationBackend struct {
	database.Backend
	store *upgradePreparationStore
}

func (b upgradePreparationBackend) RBACStore() rbacstore.Store { return b.store }

type upgradePreparationStore struct {
	rbacstore.Store
	workspace             uuid.UUID
	architecture, version string
	err                   error
	ctx                   context.Context
	node                  uuid.UUID
	reads                 int
}

func (s *upgradePreparationStore) UpgradeNode(ctx context.Context, node uuid.UUID) database.Row {
	s.ctx, s.node = ctx, node
	s.reads++
	return upgradePreparationRow{s}
}

type upgradePreparationRow struct{ store *upgradePreparationStore }

func (r upgradePreparationRow) Scan(values ...any) error {
	s := r.store
	if s.err != nil {
		return s.err
	}
	*values[0].(*uuid.UUID), *values[1].(*string), *values[2].(*string) = s.workspace, s.architecture, s.version
	return nil
}

func upgradePreparationCatalog(t *testing.T) *releasecatalog.Catalog {
	t.Helper()
	path := filepath.Join(t.TempDir(), "releases.json")
	if err := os.WriteFile(path, []byte(`{"releases":[{"version":"2.0.0","architecture":"amd64","package_sha256":"`+strings.Repeat("43", 32)+`"},{"version":"2.0.0","architecture":"arm64","package_sha256":"`+strings.Repeat("44", 32)+`"},{"version":"1.0.0","architecture":"amd64","package_sha256":"`+strings.Repeat("45", 32)+`"}]}`), 0600); err != nil {
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
		readErr, prepareErr, approvalErr     error
	}{
		{name: "amd64", target: "2.0.0", architecture: "amd64", observed: "1.2.0"},
		{name: "arm64-and-trim", target: " 2.0.0 ", architecture: "arm64", observed: "1.2.0"},
		{name: "invalid-before-read", target: "https://untrusted/release", readErr: database.ErrNotFound, prepareErr: ErrInvalidRequest, approvalErr: ErrInvalidRequest},
		{name: "missing-node", target: "2.0.0", readErr: database.ErrNotFound, prepareErr: ErrNodeUnavailable, approvalErr: ErrNodeUnavailable},
		{name: "read-failure", target: "2.0.0", readErr: database.ErrPermission, prepareErr: ErrNodeUnavailable, approvalErr: ErrNodeUnavailable},
		{name: "canceled-read", target: "2.0.0", readErr: context.Canceled, prepareErr: ErrNodeUnavailable, approvalErr: ErrNodeUnavailable},
		{name: "workspace-before-architecture", target: "2.0.0", foreign: true, prepareErr: ErrNodeUnavailable, approvalErr: ErrNodeUnavailable},
		{name: "architecture-before-catalog", target: "3.0.0", observed: "unknown", prepareErr: ErrUpgradeArchitectureUnknown, approvalErr: ErrUpgradeArchitectureUnknown},
		{name: "unsupported-architecture", target: "2.0.0", architecture: "riscv64", observed: "1.0.0", prepareErr: ErrUpgradeReleaseNotTrusted, approvalErr: ErrUpgradeReleaseNotTrusted},
		{name: "catalog-before-version", target: "3.0.0", architecture: "amd64", prepareErr: ErrUpgradeReleaseNotTrusted, approvalErr: ErrUpgradeReleaseNotTrusted},
		{name: "no-catalog", target: "2.0.0", architecture: "amd64", noCatalog: true, prepareErr: ErrUpgradeReleaseNotTrusted, approvalErr: ErrUpgradeReleaseNotTrusted},
		{name: "unobserved-version", target: "2.0.0", architecture: "amd64", prepareErr: ErrUpgradeTargetNotNewer},
		{name: "unknown-version", target: "2.0.0", architecture: "amd64", observed: "unknown", prepareErr: ErrUpgradeTargetNotNewer},
		{name: "same-version", target: "2.0.0", architecture: "amd64", observed: "2.0.0", prepareErr: ErrUpgradeTargetNotNewer},
		{name: "downgrade", target: "1.0.0", architecture: "amd64", observed: "1.2.0", prepareErr: ErrUpgradeTargetNotNewer},
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
			hash, summary, err := service.AgentUpgradeApprovalBinding(ctx, workspace, node, tc.target)
			if !errors.Is(err, tc.approvalErr) {
				t.Fatalf("approval: %v want %v", err, tc.approvalErr)
			}
			if tc.approvalErr != nil && (hash != nil || summary != nil) {
				t.Fatal("failed binding exposed approval material")
			}
			if tc.prepareErr == nil {
				if target.Version != strings.TrimSpace(tc.target) || target.Architecture != tc.architecture {
					t.Fatalf("target: %+v", target)
				}
				wantHash, wantSummary := approvals.AgentUpgradeBinding(node, target.Version, target.PackageSHA256[:], target.Architecture)
				if !bytes.Equal(hash, wantHash) || !bytes.Equal(summary, wantSummary) {
					t.Fatal("approval and command preparation resolved different identities")
				}
				// Mutating the returned digest cannot change the immutable catalog.
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
		if !errors.Is(err, ErrNodeUnavailable) || !errors.Is(err, database.ErrUnsupported) {
			t.Fatal(err)
		}
	})
}

func TestAgentUpgradePreparationBindingGolden(t *testing.T) {
	node := uuid.MustParse("019fde50-2222-7222-8222-222222222222")
	workspace := uuid.Must(uuid.NewV7())
	service := NewBackend(upgradePreparationBackend{store: &upgradePreparationStore{workspace: workspace, architecture: "amd64", version: "1.2.0"}}, 50, nil)
	service.EnableReleaseCatalog(upgradePreparationCatalog(t))
	hash, summary, err := service.AgentUpgradeApprovalBinding(t.Context(), workspace, node, " 2.0.0 ")
	want := `{"action":"agent.upgrade","architecture":"amd64","node_id":"019fde50-2222-7222-8222-222222222222","package_sha256":"4343434343434343434343434343434343434343434343434343434343434343","target_version":"2.0.0"}`
	wantHash := sha256.Sum256(append([]byte("ocservia/approval-request/agent-upgrade/v1\x00"), want...))
	if err != nil || string(summary) != want || !bytes.Equal(hash, wantHash[:]) {
		t.Fatalf("binding: %s %x %v", summary, hash, err)
	}
}
