package database_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAccessInventoryDispositionComplete(t *testing.T) {
	root := "../../.."
	inventory, err := os.ReadFile(filepath.Join(root, "docs/database-access-files.txt"))
	if err != nil {
		t.Fatal(err)
	}
	dispositions, err := os.ReadFile(filepath.Join(root, "docs/database-access-disposition.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, path := range strings.Fields(string(inventory)) {
		want[path] = true
	}
	lines := strings.Split(strings.TrimSpace(string(dispositions)), "\n")
	if lines[0] != "path\tdisposition\tboundary" {
		t.Fatal("invalid disposition header")
	}
	for _, line := range lines[1:] {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[1] == "" || fields[2] == "" || !want[fields[0]] {
			t.Fatalf("invalid, duplicate or unexpected disposition: %s", line)
		}
		if _, err := os.Stat(filepath.Join(root, fields[0])); err != nil {
			t.Fatal(err)
		}
		delete(want, fields[0])
	}
	for path := range want {
		t.Errorf("unclosed inventory entry: %s", path)
	}
}

// PR-07 closes the temporary baseline: business code has no driver allowance.
func TestNoNewBusinessDriverLeaks(t *testing.T) {
	root := "../../.."
	counts := map[string]int{}
	err := filepath.WalkDir(filepath.Join(root, "control-plane/internal"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, "control-plane/internal/database/postgres/") || strings.HasPrefix(rel, "control-plane/internal/database/mysql/") {
			return nil
		}
		// The composition root selects drivers and owns migration connections;
		// application wiring and business services still may not import them.
		if rel == "control-plane/internal/database/connection/connection.go" {
			return nil
		}
		// This cross-package fixture is imported only by tests. Production
		// imports of it are rejected below rather than grandfathered in.
		if rel == "control-plane/internal/attestationtest/helper.go" || strings.HasPrefix(rel, "control-plane/internal/database/semantictest/") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		aliases := map[string]string{}
		for _, imp := range file.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasSuffix(name, "/internal/attestationtest") || strings.HasSuffix(name, "/database/semantictest") {
				t.Errorf("test fixture imported by business code: %s", rel)
			}
			if !strings.HasPrefix(name, "github.com/jackc/") && !strings.HasPrefix(name, "database/sql") && !strings.Contains(name, "/database/postgres") && !strings.Contains(name, "/database/mysql") && !strings.Contains(name, "go-sql-driver") {
				continue
			}
			counts[rel+":"+strconv.Quote(name)]++
			base := filepath.Base(name)
			if name == "github.com/jackc/pgx/v5" {
				base = "pgx"
			}
			alias := base
			if imp.Name != nil {
				alias = imp.Name.Name
			}
			if alias == "." {
				t.Errorf("dot driver import: %s", rel)
			}
			aliases[alias] = base
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if method, ok := call.Fun.(*ast.SelectorExpr); ok {
					// URL.Query has no arguments and is not a database operation.
					if method.Sel.Name == "Exec" || method.Sel.Name == "QueryRow" || (method.Sel.Name == "Query" && len(call.Args) > 0) {
						t.Errorf("raw SQL call outside a database adapter: %s: %s", rel, method.Sel.Name)
					}
				}
			}
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := selector.X.(*ast.Ident)
			if ok && aliases[id.Name] != "" {
				counts[rel+":"+aliases[id.Name]+"."+selector.Sel.Name]++
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for key, count := range counts {
		t.Errorf("business driver leak %s: %d references; use a domain Store", key, count)
	}
}
