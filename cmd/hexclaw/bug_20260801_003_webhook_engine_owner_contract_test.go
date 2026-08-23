package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// TestBUG20260801003WebhookEngineMessageUsesPersistedEventOwner 通过 AST 锁定
// 通用 Webhook 最终送入 Engine 的消息所有者，避免再次退化为硬编码系统身份。
func TestBUG20260801003WebhookEngineMessageUsesPersistedEventOwner(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate current test file")
	}
	mainFile := filepath.Join(filepath.Dir(testFile), "main.go")
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, mainFile, nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	callbackCount := 0
	messageCount := 0
	usesEventOwner := false
	hasLegacyHardcode := false

	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "SetHandler" || len(call.Args) != 1 {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if !ok || receiver.Name != "webhookMgr" {
			return true
		}
		callback, ok := call.Args[0].(*ast.FuncLit)
		if !ok {
			return true
		}

		callbackCount++
		ast.Inspect(callback.Body, func(callbackNode ast.Node) bool {
			if literal, ok := callbackNode.(*ast.BasicLit); ok && literal.Kind == token.STRING {
				value, unquoteErr := strconv.Unquote(literal.Value)
				if unquoteErr == nil && value == "webhook-system" {
					hasLegacyHardcode = true
				}
			}

			message, ok := callbackNode.(*ast.CompositeLit)
			if !ok {
				return true
			}
			messageType, ok := message.Type.(*ast.SelectorExpr)
			if !ok || messageType.Sel.Name != "Message" {
				return true
			}
			qualifier, ok := messageType.X.(*ast.Ident)
			if !ok || qualifier.Name != "adapter" {
				return true
			}

			messageCount++
			for _, element := range message.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := field.Key.(*ast.Ident)
				if !ok || key.Name != "UserID" {
					continue
				}
				owner, ok := field.Value.(*ast.SelectorExpr)
				if !ok || owner.Sel.Name != "UserID" {
					continue
				}
				ownerSource, ok := owner.X.(*ast.Ident)
				if ok && ownerSource.Name == "event" {
					usesEventOwner = true
				}
			}
			return true
		})
		return false
	})

	if callbackCount != 1 {
		t.Errorf("generic webhook SetHandler callback count=%d, want 1", callbackCount)
	}
	if messageCount != 1 {
		t.Errorf("adapter.Message count in generic webhook callback=%d, want 1", messageCount)
	}
	if !usesEventOwner {
		t.Error("generic webhook adapter.Message.UserID must be event.UserID")
	}
	if hasLegacyHardcode {
		t.Error("generic webhook callback must not contain the webhook-system identity")
	}
}
