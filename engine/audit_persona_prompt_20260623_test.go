package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// AUDIT bug#7 2026-06-23：@智能体（如「专业翻译」）时，Agent 人设被正确应用（不是路由 bug），
// 但用户问"你都能做什么"且无具体任务时，弱模型(glm-4-flash)会逐字复述系统指令（"系统指令：文档翻译…"）。
// 修复：给 Agent 派生的 system prompt 追加防复述守则，让模型用自己的话作答、不逐字复述设定。
func TestAudit_AgentPromptHasAntiRecitationGuard_20260623(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"test": {APIKey: "sk-test", Model: "test-model"}}
	router, _ := llmrouter.New(cfg.LLM)
	dir := t.TempDir()
	store, _ := sqlitestore.New(filepath.Join(dir, "test.db"))
	t.Cleanup(func() { store.Close() })
	store.Init(context.Background())
	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())

	meta := map[string]string{"agent_prompt": "你是专业文档翻译。用户指定源语言、目标语言及原文，你输出译文。"}
	msgs := eng.buildStreamMessages(context.Background(), "", nil, "", "你都能做什么", meta, nil)
	sys := msgs[0].Content

	if !strings.Contains(sys, "你是专业文档翻译") {
		t.Fatalf("agent_prompt 未被应用")
	}
	if !strings.Contains(sys, "不要逐字复述") {
		t.Errorf("BUG#7: Agent system prompt 缺少防复述守则 → 弱模型会把系统指令原样吐出来")
	}
}

// AUDIT bug#4 2026-06-23：默认人设(小蟹)缺少官网与产品定位 → 问"本应用官网"时模型去网络搜索，
// 搜到同名"美甲/HexClaw nail"站点答错。人设里钉死官网地址 + 产品定位，使其无需搜索即可正确作答。
// 修复前 git HEAD 的 react.go 不含 "hexclaw.net"（grep -c=0，已取证）。
func TestAudit_PersonaContainsOfficialSite_20260623(t *testing.T) {
	p := DefaultSystemPrompt()
	if !strings.Contains(p, "hexclaw.net") {
		t.Errorf("BUG#4: 默认人设缺官网地址 hexclaw.net → 问官网会乱搜")
	}
	// 必须明确告知"问到官网直接回答，不要去搜索"，否则弱模型仍可能 web_search 出错站点。
	if !strings.Contains(p, "官网") {
		t.Errorf("BUG#4: 默认人设未提及官网语义锚点")
	}
}

// AUDIT bug#6 2026-06-23：用户明确要"可下载的 PDF 文档"却只拿到 markdown 产物、不知如何导出 PDF。
// 桌面端产物卡片 Download 下拉支持 8 种格式（含 PDF）；修复点：人设需指引模型在用户点名某格式时
// 明确告诉用户去下拉里选该格式，而不是笼统说"已生成 markdown 产物"。
func TestAudit_PersonaGuidesExplicitFormatExport_20260623(t *testing.T) {
	p := DefaultSystemPrompt()
	if !strings.Contains(p, "明确点名要某种可下载格式") {
		t.Errorf("BUG#6: 人设缺少『用户点名某格式时如何指引导出』的说明")
	}
	// 指引里要点到 Download 下拉（导出入口），让用户拿到 PDF。
	if !strings.Contains(p, "Download") {
		t.Errorf("BUG#6: 指引未指向产物卡片 Download 导出入口")
	}
}
