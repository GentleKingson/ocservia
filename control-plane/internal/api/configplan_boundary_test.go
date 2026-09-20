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

	"github.com/GentleKingson/ocservia/control-plane/internal/api/configplanhttp"
)

func TestConfigPlanHTTPBoundary(t *testing.T) {
	const internal = "github.com/GentleKingson/ocservia/control-plane/internal/"
	allowed := map[string]map[string]bool{
		internal + "api/httpx":  {"Registrar": true, "WriteJSON": true, "WriteProblem": true, "ParseUUIDv7": true, "DecodeStrictJSON": true, "RequireIdempotencyKey": true},
		internal + "configplan": {"CreateRequest": true, "ApplyRequest": true, "Plan": true, "Template": true, "ErrInvalid": true, "ErrStaleRevision": true, "ErrCapability": true},
		internal + "operations": {"Operation": true, "ErrInvalidRequest": true, "ErrStaleRevision": true, "ErrCapabilityMissing": true, "ErrIdempotencyConflict": true, "ErrConfigApplyActive": true},
		internal + "approvals":  {"ErrNotReady": true},
		internal + "database":   {"ErrNotFound": true},
	}
	files, err := filepath.Glob("configplanhttp/*.go")
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
			if path == "reflect" || path == "unsafe" || strings.HasPrefix(path, "database/") {
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
	wantFields := map[string]string{"plans": "configplanhttp.Plans", "requestInfo": "func(*http.Request) configplanhttp.RequestInfo", "secretUse": "configplanhttp.SecretUse"}
	handler := reflect.TypeOf(configplanhttp.Handler{})
	if handler.NumField() != len(wantFields) {
		t.Fatal("ConfigPlan HTTP dependency set changed")
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
		{reflect.TypeOf((*configplanhttp.Plans)(nil)).Elem(), []string{"Apply", "Create", "Get"}},
		{reflect.TypeOf((*configPlanLookup)(nil)).Elem(), []string{"ApprovalBinding", "Resource"}},
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
		if server.Field(i).Type.String() == "*configplan.Service" {
			t.Fatal("parent retains full ConfigPlan service")
		}
	}
	field, ok := server.FieldByName("configPlanLookup")
	if !ok || field.Type != reflect.TypeOf((*configPlanLookup)(nil)).Elem() {
		t.Fatal("parent must store the narrow lookup")
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
				case "createConfigPlan", "getConfigPlan", "applyConfigPlan", "writeConfigPlanError":
					t.Errorf("%s: old HTTP implementation %s remains", name, fn.Name.Name)
				}
			}
		}
	}
	// TestNodeHTTPBoundary independently prohibits all business/auth imports in httpx.
}
