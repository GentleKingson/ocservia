package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/api/useroperationshttp"
)

func TestUserOperationsHTTPBoundary(t *testing.T) {
	const internal = "github.com/GentleKingson/ocservia/control-plane/internal/"
	allowed := map[string]map[string]bool{
		internal + "api/httpx":      {"Registrar": true, "WriteJSON": true, "WriteProblem": true, "ParseUUIDv7": true, "DecodeStrictJSON": true, "RequireIdempotencyKey": true},
		internal + "useroperations": {"Policy": true, "PolicyRequest": true, "Batch": true, "BatchRequest": true, "BatchItemRequest": true, "Metrics": true, "ErrInvalidRequest": true, "ErrVersionConflict": true, "ErrIdempotencyConflict": true, "ErrNotFound": true},
		internal + "auth":           {"Principal": true},
		internal + "rbac":           {"Resource": true, "ErrForbidden": true},
		internal + "approvals":      {"ErrNotReady": true},
		internal + "database":       {"ErrNotFound": true},
	}
	files, err := filepath.Glob("useroperationshttp/*.go")
	if err != nil || len(files) == 0 {
		t.Fatal("missing module", err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		aliases := map[string]string{}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if path == "reflect" || path == "unsafe" || strings.HasPrefix(path, "database/") || strings.HasPrefix(path, "github.com/jackc/") || strings.HasPrefix(path, "github.com/go-sql-driver/") {
				t.Errorf("%s: forbidden dependency %s", name, path)
			}
			if !strings.HasPrefix(path, internal) {
				continue
			}
			if allowed[path] == nil {
				t.Errorf("%s: forbidden dependency %s", name, path)
			}
			alias := filepath.Base(path)
			if imp.Name != nil {
				alias = imp.Name.Name
			}
			if alias == "." || alias == "_" {
				t.Errorf("%s: hidden dependency %s", name, path)
			}
			aliases[alias] = path
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if _, ok := n.(*ast.TypeAssertExpr); ok {
				t.Errorf("%s: capability recovery by assertion", name)
			}
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && aliases[id.Name] != "" && !allowed[aliases[id.Name]][sel.Sel.Name] {
					t.Errorf("%s: forbidden capability %s.%s", name, id.Name, sel.Sel.Name)
				}
			}
			return true
		})
	}
	wantFields := map[string]string{"operations": "useroperationshttp.Operations", "authorizer": "useroperationshttp.Authorizer", "requestInfo": "func(*http.Request) useroperationshttp.RequestInfo", "logger": "*slog.Logger"}
	handler := reflect.TypeOf(useroperationshttp.Handler{})
	if handler.NumField() != len(wantFields) {
		t.Fatal("UserOperations HTTP dependency set changed")
	}
	for i := 0; i < handler.NumField(); i++ {
		field := handler.Field(i)
		if field.Type.String() != wantFields[field.Name] {
			t.Errorf("module stores %s %s", field.Name, field.Type)
		}
	}
	for _, tc := range []struct {
		port    reflect.Type
		methods []string
	}{
		{reflect.TypeOf((*useroperationshttp.Operations)(nil)).Elem(), []string{"CreateBatch", "GetBatch", "GetPolicy", "Metrics", "SetPolicy"}},
		{reflect.TypeOf((*useroperationshttp.Authorizer)(nil)).Elem(), []string{"Authorize", "Node"}},
	} {
		var got []string
		for i := 0; i < tc.port.NumMethod(); i++ {
			got = append(got, tc.port.Method(i).Name)
		}
		if !reflect.DeepEqual(got, tc.methods) {
			t.Errorf("%s capabilities: %v", tc.port, got)
		}
	}
	server := reflect.TypeOf(Server{})
	for i := 0; i < server.NumField(); i++ {
		if server.Field(i).Type.String() == "*useroperations.Service" {
			t.Fatal("parent retains full UserOperations service")
		}
	}
	files, err = filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				switch fn.Name.Name {
				case "getUserPolicy", "setUserPolicy", "createUserBatch", "getUserBatch", "userOperationMetrics", "writeUserOperationsError":
					t.Errorf("%s: old HTTP implementation %s remains", name, fn.Name.Name)
				}
			}
		}
	}
	// TestNodeHTTPBoundary independently prohibits all business/auth imports in httpx.
}
