package database_test

import (
	"bufio"
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

// The temporary baseline is a ratchet, not permission for new driver usage.
// Removed references should be removed from it in the owning follow-up PR.
func TestNoNewBusinessDriverLeaks(t *testing.T) {
	root := "../../.."
	f, err := os.Open(filepath.Join(root, "docs/database-driver-baseline.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	allowed := map[string]int{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			t.Fatalf("invalid baseline: %s", scanner.Text())
		}
		count, err := strconv.Atoi(fields[0])
		if err != nil {
			t.Fatal(err)
		}
		allowed[fields[1]] = count
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	err = filepath.WalkDir(filepath.Join(root, "control-plane/internal"), func(path string, entry fs.DirEntry, err error) error {
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
		if strings.HasPrefix(rel, "control-plane/internal/database/postgres/") {
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
			if !strings.HasPrefix(name, "github.com/jackc/") && !strings.HasPrefix(name, "database/sql") && !strings.Contains(name, "/database/postgres") && !strings.Contains(name, "go-sql-driver") {
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
		if count > allowed[key] {
			t.Errorf("new driver leak %s: %d > %d; use a domain Store", key, count, allowed[key])
		}
	}
}
