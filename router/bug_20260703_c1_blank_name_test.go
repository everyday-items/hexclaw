package router

import (
	"strings"
	"testing"
)

// BUG-20260703 C1（未覆盖场景轮抓出）：Agent 注册/更新只拦 Name=="" 空串，
// 纯空白名「"   "」绕过校验被存成空白主键（PRIMARY KEY 空白串），且 UI 直连 API /
// ChannelAgentBinding 无 trim 保护。契约：名称按 TrimSpace 后为空即拒绝。
func TestBug20260703_C1_RegisterRejectsBlankName(t *testing.T) {
	for _, name := range []string{" ", "   ", "\t", "\n", "  \t \n "} {
		r := New()
		if err := r.Register(AgentConfig{Name: name}); err == nil {
			t.Errorf("C1: 纯空白名 %q 应被拒绝，却注册成功", name)
		}
	}
}

func TestBug20260703_C1_UpdateRejectsBlankName(t *testing.T) {
	r := New()
	if err := r.Register(AgentConfig{Name: "real"}); err != nil {
		t.Fatalf("前置注册失败: %v", err)
	}
	if err := r.UpdateAgent(AgentConfig{Name: "   "}); err == nil {
		t.Error("C1: UpdateAgent 纯空白名应被拒绝")
	}
	// 正常名不受影响
	if err := r.UpdateAgent(AgentConfig{Name: "real", Model: "m"}); err != nil {
		t.Errorf("C1: 合法名 update 不应受影响: %v", err)
	}
}

// 防御性锚点：非空白正常名照常放行（避免过度收紧）。
func TestBug20260703_C1_NormalNameStillOK(t *testing.T) {
	r := New()
	if err := r.Register(AgentConfig{Name: strings.TrimSpace("  coder  ")}); err != nil {
		t.Errorf("已 trim 的正常名应放行: %v", err)
	}
}
