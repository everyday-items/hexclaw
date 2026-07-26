package usecase

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionCreateGradingJobInputsFreezeOneRouteAndParentBudgetSource(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	constructions := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			ident, ok := literal.Type.(*ast.Ident)
			if !ok || ident.Name != "CreateGradingJobInput" {
				return true
			}
			constructions++
			counts := map[string]int{}
			for _, element := range literal.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := field.Key.(*ast.Ident); ok {
					counts[key.Name]++
				}
			}
			for _, field := range []string{
				"ModelSnapshot",
				"ParentAutomaticAttemptID",
				"ParentAutomaticDeadlineAt",
			} {
				if counts[field] != 1 {
					t.Errorf("%s CreateGradingJobInput has %d %s assignments, want exactly one",
						name, counts[field], field)
				}
			}
			return true
		})
	}
	if constructions == 0 {
		t.Fatal("no production CreateGradingJobInput construction found")
	}
}
