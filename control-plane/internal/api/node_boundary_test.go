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

	"github.com/GentleKingson/ocservia/control-plane/internal/api/nodehttp"
)

func TestNodeHTTPBoundary(t *testing.T) {
	const internal = "github.com/GentleKingson/ocservia/control-plane/internal/"
	// Allow shared values and errors, not the stores/providers living beside them.
	allowed := map[string]map[string]bool{
		internal + "api/httpx":      {"WriteJSON": true, "WriteProblem": true, "PageSize": true, "ParseOptionalUUIDv7": true},
		internal + "database":       {"ErrNotFound": true},
		internal + "database/value": {"Timestamp": true, "ParseTimestamp": true},
		internal + "telemetry":      {"Node": true, "IPBan": true, "HistoryPoint": true, "ErrInvalidMetric": true, "ErrInvalidResolution": true},
		internal + "telemetryread":  {"Session": true},
	}
	for _, dir := range []string{"nodehttp", "httpx"} {
		files, err := filepath.Glob(dir + "/*.go")
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("missing module %s", dir)
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
				if !strings.HasPrefix(path, internal) {
					continue
				}
				if dir == "httpx" || allowed[path] == nil {
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
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && aliases[id.Name] != "" && !allowed[aliases[id.Name]][sel.Sel.Name] {
						t.Errorf("%s: forbidden capability %s.%s", name, id.Name, sel.Sel.Name)
					}
				}
				return true
			})
		}
	}
	wantFields := map[string]string{"reader": "nodehttp.Reader", "logger": "*slog.Logger", "workspace": "func(*http.Request) uuid.UUID"}
	handler := reflect.TypeOf(nodehttp.Handler{})
	if handler.NumField() != len(wantFields) {
		t.Fatal("node handler dependency set changed")
	}
	for i := 0; i < handler.NumField(); i++ {
		f := handler.Field(i)
		if f.Type.String() != wantFields[f.Name] {
			t.Errorf("node handler stores %s %s", f.Name, f.Type)
		}
	}
	wantMethods := []string{"GetNode", "HistoryFrom", "ListIPBans", "ListNodesInWorkspace", "ListSessions"}
	reader := reflect.TypeOf((*nodehttp.Reader)(nil)).Elem()
	var methods []string
	for i := 0; i < reader.NumMethod(); i++ {
		methods = append(methods, reader.Method(i).Name)
	}
	if !reflect.DeepEqual(methods, wantMethods) {
		t.Fatalf("Reader capabilities: %v", methods)
	}
	files, err := filepath.Glob("*.go")
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
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil {
				continue
			}
			switch fn.Name.Name {
			case "listNodes", "getNode", "listNodeSessions", "listNodeIPBans", "listNodeTelemetry", "nodePathID":
				t.Errorf("%s: old API handler %s remains", name, fn.Name.Name)
			}
		}
	}
	server := reflect.TypeOf(Server{})
	for i := 0; i < server.NumField(); i++ {
		if server.Field(i).Type.String() == "*telemetry.Service" {
			t.Fatal("duplicate telemetry service state")
		}
	}
}
