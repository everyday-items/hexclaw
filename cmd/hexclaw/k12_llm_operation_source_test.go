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
		"practiceGenFn":            false,
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
		if name.Name == "visionFn" {
			builderCalls := 0
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if ok && ident.Name == "completeK12VisionRequest" {
					builderCalls++
				}
				return true
			})
			if builderCalls != 1 {
				t.Errorf("visionFn canonical K12 vision builder calls = %d, want 1", builderCalls)
			}
			required[name.Name] = true
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
	assertK12VisionBuilderMarksCompleteNonIdempotent(t)
}

// K12-PROJECTING-FROZEN-ROUTE-001：所有页面摘要文本生成器都在同一个已确认的
// GradingJob 上下文中调用，因此必须共用同一个路由解析器，不能各自重新读取
// 也不能回退到 router.Default()。
func TestK12ProjectionTextClosuresShareFrozenRouteResolver(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{
		"tutoringTipsReviewGenFn":  false,
		"parentTeachingGuideGenFn": false,
		"workFeedbackGenFn":        false,
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
		resolverCalls := 0
		modelFields := 0
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			if call, ok := inner.(*ast.CallExpr); ok {
				if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "resolveK12FrozenTextCompletionRoute" {
					resolverCalls++
				}
			}
			literal, ok := inner.(*ast.CompositeLit)
			if !ok {
				return true
			}
			typ, ok := literal.Type.(*ast.SelectorExpr)
			if !ok || typ.Sel.Name != "CompletionRequest" {
				return true
			}
			for _, element := range literal.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, keyOK := field.Key.(*ast.Ident)
				value, valueOK := field.Value.(*ast.Ident)
				if keyOK && valueOK && key.Name == "Model" && value.Name == "model" {
					modelFields++
				}
			}
			return true
		})
		if resolverCalls != 1 {
			t.Errorf("%s shared frozen-route resolver calls=%d, want 1", name.Name, resolverCalls)
		}
		if modelFields != 1 {
			t.Errorf("%s CompletionRequest.Model=model fields=%d, want 1", name.Name, modelFields)
		}
		required[name.Name] = true
		return false
	})
	for name, found := range required {
		if !found {
			t.Errorf("K12 projecting text closure %s not found", name)
		}
	}
}

func assertK12VisionBuilderMarksCompleteNonIdempotent(t *testing.T) {
	t.Helper()
	source, err := os.ReadFile("k12_vision_request.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "k12_vision_request.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
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
		calls++
		if len(call.Args) == 0 || !isK12NoReplayContextCall(call.Args[0]) {
			t.Error("completeK12VisionRequest provider.Complete is missing k12NonIdempotentLLMContext")
		}
		return true
	})
	if calls != 1 {
		t.Errorf("completeK12VisionRequest provider.Complete calls = %d, want 1", calls)
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
