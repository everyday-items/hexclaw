package records

// BUG-20260712-#2 未注册 agent 写记录 → 原始 SQL 错误甩成 500：K12 批改判错后写错题记录时，若
// agent_name 外键指向的 agent 未注册（agent_records.agent_name REFERENCES agents(name)），
// SQLite 抛 "FOREIGN KEY constraint failed"，此前直接 %w 冒泡成 HTTP 500，用户看不懂。
// 真机复现：隔离新库未注册 agent → 批改返回 `{"error":"usecase: 错题入库: ... FOREIGN KEY
// constraint failed (787)"}` 500。
//
// 修复：Store.Put 识别外键失败 → 返回语义错误 ErrScopeNotFound + 清晰中文提示 → 上层映射 400。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestPut_UnregisteredAgentReturnsScopeNotFound 未注册 agent 写记录 → ErrScopeNotFound（非裸 SQL 错误）。
// 复用 newFKStore（开启 foreign_keys，只注册 mingming，与生产 _pragma=foreign_keys(1) 一致）。
func TestPut_UnregisteredAgentReturnsScopeNotFound(t *testing.T) {
	s := newFKStore(t)
	_, err := s.Put(context.Background(), &AgentRecord{
		AgentName: "查无此辅导助手", Collection: "ladder", Status: "new", SourceSession: "x",
	})
	if err == nil {
		t.Fatal("未注册 agent 写记录应报错")
	}
	if !errors.Is(err, ErrScopeNotFound) {
		t.Fatalf("BUG 复现：未注册 agent 应返回语义错误 ErrScopeNotFound（上层映射 400 清晰提示），却返回：%v", err)
	}
	if !strings.Contains(err.Error(), "未注册") {
		t.Fatalf("错误提示应包含清晰的「未注册」，got：%v", err)
	}
	// 已注册的 agent 正常写入（不误伤）。
	if _, err := s.Put(context.Background(), &AgentRecord{
		AgentName: "mingming", Collection: "ladder", Status: "new", SourceSession: "ok",
	}); err != nil {
		t.Fatalf("已注册 agent 应正常写入：%v", err)
	}
}
