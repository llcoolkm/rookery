package codex_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The provider may accept Host in the factory signature, but may retain only
// individual transport/process/path facilities. This gate applies to all OSes.
func TestCodexHasNoCommandExecutionDependency(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		name := file.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		platformAlias := "platform"
		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if path == "os/exec" || path == "syscall" || path == "plugin" {
				t.Errorf("%s imports command facility %s", name, path)
			}
			if path == "github.com/llcoolkm/rookery/go/platform" && spec.Name != nil {
				platformAlias = spec.Name.Name
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				switch node.Sel.Name {
				case "Exec", "Executor", "Spec", "ExecResult", "ExecState", "Executable":
					t.Errorf("%s depends on command facility %s", name, node.Sel.Name)
				}
			case *ast.StructType:
				for _, field := range node.Fields.List {
					if sel, ok := field.Type.(*ast.SelectorExpr); ok {
						if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == platformAlias && sel.Sel.Name == "Host" {
							t.Errorf("%s retains platform.Host and its command facility", name)
						}
					}
				}
			}
			return true
		})
	}
}
