package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

func TestK12ProductionLLMClosuresMarkEveryCompleteAsNonIdempotent(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{
		"visionFn":                 false,
		"retryGenFn":               false,
		"tutoringTipsReviewGenFn":  false,
		"parentTeachingGuideGenFn": false,
		"causeSummaryGenFn":        false,
		"workFeedbackGenFn":        false,
		"workFeedbackVisionFn":     false,
	}
	ast.Inspect(file, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		name, ok := assign.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		if _, tracked := required[name.Name]; !tracked {
			return true
		}
		fn, ok := assign.Rhs[0].(*ast.FuncLit)
		if !ok {
			t.Errorf("%s is not a function literal", name.Name)
			return false
		}
		completeCalls := 0
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Complete" {
				return true
			}
			receiver, ok := selector.X.(*ast.Ident)
			if !ok || receiver.Name != "provider" {
				return true
			}
			completeCalls++
			if len(call.Args) == 0 || !isK12NoReplayContextCall(call.Args[0]) {
				t.Errorf("%s provider.Complete is missing k12NonIdempotentLLMContext", name.Name)
			}
			return true
		})
		if completeCalls != 1 {
			t.Errorf("%s provider.Complete calls = %d, want 1", name.Name, completeCalls)
		}
		required[name.Name] = true
		return false
	})
	for name, found := range required {
		if !found {
			t.Errorf("K12 production LLM closure %s not found", name)
		}
	}
}

func isK12NoReplayContextCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == "k12NonIdempotentLLMContext" && len(call.Args) == 1
}
