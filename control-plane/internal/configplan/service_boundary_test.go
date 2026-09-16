package configplan

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestConsumerServiceBoundaries(t *testing.T) {
	const internal = "github.com/GentleKingson/ocservia/control-plane/internal/"
	for _, boundary := range []struct {
		pkg, field, port, method, provider string
		constructors                       map[string]int
	}{
		{"configplan", "operations", "operationCreator", "CreateSynthetic", "operations", map[string]int{"NewBackend": 1}},
		{"useroperations", "users", "userMutator", "Mutate", "userstate", map[string]int{"NewBackend": 1, "NewWithConcurrencyBackend": 1}},
	} {
		t.Run(boundary.pkg, func(t *testing.T) {
			files, err := filepath.Glob("../" + boundary.pkg + "/*.go")
			if err != nil {
				t.Fatal(err)
			}
			foundField, foundPort := false, false
			foundConstructors := map[string]bool{}
			for _, path := range files {
				if strings.HasSuffix(path, "_test.go") {
					continue
				}
				file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
				if err != nil {
					t.Fatal(err)
				}
				providers := map[string]bool{}
				for _, imp := range file.Imports {
					name, err := strconv.Unquote(imp.Path.Value)
					if err != nil {
						t.Fatal(err)
					}
					for _, forbidden := range []string{internal + "api", internal + "platform/app"} {
						if name == forbidden || strings.HasPrefix(name, forbidden+"/") {
							t.Errorf("%s imports assembly/HTTP package %s", path, name)
						}
					}
					if name == internal+boundary.provider {
						alias := boundary.provider
						if imp.Name != nil {
							alias = imp.Name.Name
						}
						if alias == "." || alias == "_" {
							t.Errorf("%s hides provider import", path)
						}
						providers[alias] = true
					}
				}
				ast.Inspect(file, func(node ast.Node) bool {
					// The only concrete Service reference permitted in production is
					// the compile-time implementation assertion, never a cast or field.
					if spec, ok := node.(*ast.ValueSpec); ok && len(spec.Names) == 1 && spec.Names[0].Name == "_" {
						if id, ok := spec.Type.(*ast.Ident); ok && id.Name == boundary.port {
							return false
						}
					}
					if sel, ok := node.(*ast.SelectorExpr); ok && sel.Sel.Name == "Service" {
						if id, ok := sel.X.(*ast.Ident); ok && providers[id.Name] {
							t.Errorf("%s uses concrete %s.Service outside implementation assertion", path, id.Name)
						}
					}
					if spec, ok := node.(*ast.TypeSpec); ok {
						switch spec.Name.Name {
						case "Service":
							if structure, ok := spec.Type.(*ast.StructType); ok {
								for _, field := range structure.Fields.List {
									for _, name := range field.Names {
										if name.Name == boundary.field {
											id, ok := field.Type.(*ast.Ident)
											foundField = ok && id.Name == boundary.port
										}
									}
								}
							}
						case boundary.port:
							port, ok := spec.Type.(*ast.InterfaceType)
							foundPort = ok && len(port.Methods.List) == 1 && len(port.Methods.List[0].Names) == 1 && port.Methods.List[0].Names[0].Name == boundary.method
						}
					}
					if fn, ok := node.(*ast.FuncDecl); ok && fn.Recv == nil {
						if index, wanted := boundary.constructors[fn.Name.Name]; wanted && len(fn.Type.Params.List) > index {
							id, ok := fn.Type.Params.List[index].Type.(*ast.Ident)
							foundConstructors[fn.Name.Name] = ok && id.Name == boundary.port
						}
					}
					return true
				})
			}
			if !foundField || !foundPort {
				t.Errorf("consumer field/interface changed: field=%v minimal interface=%v", foundField, foundPort)
			}
			for name := range boundary.constructors {
				if !foundConstructors[name] {
					t.Errorf("%s must accept %s", name, boundary.port)
				}
			}
		})
	}
}
