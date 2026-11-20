package app

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestArchitectureImportBoundaries enforces the compile-time dependency rules
// documented in docs/architecture.md:
//   - Domain and HTTP packages never import assembly (internal/platform/app).
//   - Business packages stay decoupled from SQL drivers (database/postgres, pgx).
//   - No Go package imports Iroh.
func TestArchitectureImportBoundaries(t *testing.T) {
	moduleRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = moduleRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -json ./...: %v", err)
	}

	const modulePrefix = "github.com/GentleKingson/ocservia/control-plane/"
	const appPkg = modulePrefix + "internal/platform/app"

	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg struct {
			ImportPath string   `json:"ImportPath"`
			Imports    []string `json:"Imports"`
		}
		if err := dec.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		rel := strings.TrimPrefix(pkg.ImportPath, modulePrefix)
		for _, imp := range pkg.Imports {
			if strings.Contains(strings.ToLower(imp), "/iroh") || strings.HasSuffix(strings.ToLower(imp), "iroh") {
				t.Errorf("%s must not import Iroh (%s)", rel, imp)
			}
			if imp == appPkg && !strings.HasPrefix(rel, "cmd/") && rel != "internal/platform/app" {
				t.Errorf("%s must not import assembly package %s", rel, appPkg)
			}
			isDriverImport := imp == modulePrefix+"internal/database/postgres" ||
				strings.HasPrefix(imp, "github.com/jackc/pgx/")
			allowedDriverConsumer := strings.HasPrefix(rel, "cmd/") ||
				rel == "internal/platform/app" ||
				rel == "internal/database/postgres" ||
				rel == "internal/database/connection" ||
				rel == "migrations" ||
				rel == "internal/testfixture"
			if isDriverImport && !allowedDriverConsumer {
				t.Errorf("%s must stay decoupled from SQL driver package %s", rel, imp)
			}
			if imp == modulePrefix+"internal/database/connection" && !allowedDriverConsumer && rel != "internal/platform/config" {
				t.Errorf("%s must not import %s", rel, imp)
			}
		}
	}
}
