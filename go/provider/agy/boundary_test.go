package agy_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// All production files, including inactive OS files, must remain independent of
// process execution, environment scraping and direct filesystem access.
func TestAGYHasNoProcessScrapingOrCommandExecution(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range files {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range f.Imports {
			p, _ := strconv.Unquote(spec.Path.Value)
			for _, bad := range []string{"os", "os/exec", "syscall", "net", "path/filepath", "plugin"} {
				if p == bad {
					t.Errorf("%s imports forbidden host facility %s", name, p)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.BasicLit:
				if v.Kind == token.STRING {
					s, _ := strconv.Unquote(v.Value)
					if strings.Contains(s, "/proc") || strings.Contains(s, "ANTIGRAVITY_LS_ADDRESS") || strings.Contains(s, "ANTIGRAVITY_CSRF_TOKEN") {
						t.Errorf("%s accesses process data or credentials", name)
					}
				}
			case *ast.SelectorExpr:
				for _, bad := range []string{"CommandLine", "Exec", "Executor", "Spec", "Executable", "Getenv", "Environ", "ReadFile"} {
					if v.Sel.Name == bad {
						t.Errorf("%s depends on forbidden facility %s", name, bad)
					}
				}
			case *ast.StructType:
				for _, field := range v.Fields.List {
					if sel, ok := field.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Host" {
						t.Errorf("%s retains host execution capability", name)
					}
				}
			}
			return true
		})
	}
}
