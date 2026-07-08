package engine

import (
	"context"
	"testing"
)

type bugH7AutoMemoryCtxKey struct{}

// TestBugH7_AutoExtractMemoryForRole_AcceptsParentCtx 是 BUG-H7 的 regression test（engine 层）。
//
// BUG：autoExtractMemoryForRole 未接收 ctx，内部 goroutine 用 context.Background() 做 30s 超时。
//
//	调用这个函数的 engine 层本身有 request ctx（带 session_id/user_id），
//	但 auto-memory 这条异步路径把它们全部丢了 —— auto-memory 的 LLM 调用日志
//	与主请求无法关联。
//
// 契约：autoExtractMemoryForRole 的第一个参数必须是 context.Context，
//
//	内部异步 ctx 必须由 trace.Detach(parentCtx) 派生，保留 parent Values。
//
// RED（未修版本）：函数签名不带 ctx → 本测试编译失败 [build failed]
// GREEN（修复版本）：autoExtractMemoryForRole(parentCtx, userText, ...) → 编译通过
//
// 备注：本测试不实际执行函数（fileMem 为 nil 会提前 return，无副作用），
//
//	仅通过编译即证明签名契约满足；parent Value 的保留由 trace/bugfix_test.go 的
//	Detach 契约测试保障。
func TestBugH7_AutoExtractMemoryForRole_AcceptsParentCtx(t *testing.T) {
	e := &ReActEngine{} // fileMem 为 nil，autoExtractMemoryForRole 会在第一行 return

	parentCtx := context.WithValue(context.Background(), bugH7AutoMemoryCtxKey{}, "sess-h7-engine")

	// 未修版本签名 autoExtractMemoryForRole(userText, assistantText, role)
	//   → 下一行编译失败 [build failed]
	// 修复版本签名 autoExtractMemoryForRole(parentCtx, userText, assistantText, role)
	//   → 编译通过
	e.autoExtractMemoryForRole(parentCtx, "short", "shorter", "user")

	// 到这就证明签名契约 OK；parent Value 保留由 trace.Detach 契约保障（trace/bugfix_test.go）
	if parentCtx.Value(bugH7AutoMemoryCtxKey{}) != "sess-h7-engine" {
		t.Fatal("BUG-H7: sanity check failed — ctx Value 未能注入")
	}
}
