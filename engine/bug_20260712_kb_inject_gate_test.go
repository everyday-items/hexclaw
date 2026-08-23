package engine

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

// BUG-20260712-N（召回最佳实践落地 · 场景域隔离 + 寒暄门控）：
//
// ① 场景实例会话不注入**全局**知识库——真机取证：在「小王的辅导助手」会话里问天气，
//
//	命中《Go面试题new》。就算相关性完美，孩子的辅导会话也不该检索家长的个人文档库
//	（跨域污染 + 隐私面扩大）。场景包有自己的数据通道（K12 错题本/学情），全局 KB
//	自动注入只对通用会话开放；待知识集支持按 agent 绑定后再对场景实例开放。
//	显式 `@` 召唤知识不走此门，不受影响。
//
// ② 超短寒暄（<4 rune：你好/ok/1+1）无检索意图，跳过整个 embed+检索往返（纯延迟优化）。
func gateEngine(t *testing.T) *ReActEngine {
	t.Helper()
	e := &ReActEngine{}
	d := agentrouter.New()
	if err := d.Register(agentrouter.AgentConfig{
		Name:     "k12-tutor-abc",
		Metadata: map[string]string{"scenario": "k12-tutor", "avatar": "🎓"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Register(agentrouter.AgentConfig{
		Name: "translator",
	}); err != nil {
		t.Fatal(err)
	}
	e.SetAgentRouter(d)
	return e
}

func gateMsg(content string, meta map[string]string) *adapter.Message {
	return &adapter.Message{ID: "m1", Platform: adapter.PlatformAPI, UserID: "u1", ChatID: "c1", Content: content, Metadata: meta}
}

func TestBug20260712_KBGate_ScenarioAgentSkipsGlobalKB(t *testing.T) {
	e := gateEngine(t)
	cases := []map[string]string{
		{"role": "k12-tutor-abc"},
		{"pinned_agent": "k12-tutor-abc"},
	}
	for _, meta := range cases {
		if e.shouldAutoInjectKB(gateMsg("明天天气怎么样，适合户外写作业吗", meta)) {
			t.Fatalf("场景实例会话不得注入全局知识库（跨域污染），meta=%v", meta)
		}
	}
}

func TestBug20260712_KBGate_GeneralSessionsKeepKB(t *testing.T) {
	e := gateEngine(t)
	cases := []map[string]string{
		nil,                             // 默认助理
		{"role": "translator"},          // 普通 agent
		{"pinned_agent": "translator"},  // 普通 agent（锁定）
		{"role": "ghost-deleted-agent"}, // 查无此人：由 guardExplicitRoleExists fail-loud，本门不越权拦
	}
	for _, meta := range cases {
		if !e.shouldAutoInjectKB(gateMsg("公司年假制度是怎么规定的", meta)) {
			t.Fatalf("通用会话应保留全局知识库自动注入，meta=%v", meta)
		}
	}
}

func TestBug20260712_KBGate_TinyChitchatSkipsRetrieval(t *testing.T) {
	e := gateEngine(t)
	for _, q := range []string{"你好", "ok", "谢谢", "1+1"} {
		if e.shouldAutoInjectKB(gateMsg(q, nil)) {
			t.Fatalf("超短寒暄 %q 不应触发检索往返", q)
		}
	}
	if !e.shouldAutoInjectKB(gateMsg("年假怎么请", nil)) {
		t.Fatal("正常短问题（≥4 rune）应照常检索")
	}
}

func TestBug20260728_KBGate_ExplicitNoRetrievalSkipsEmbeddingWithoutKillingExplicitSearch(t *testing.T) {
	e := gateEngine(t)
	for _, q := range []string{
		"不用检索知识库，只把下面这句话改写得更自然",
		"不查询、不检索、不引用知识资料，直接回答一句问候",
		"请只回答“今天也要轻松一点”，不要解释，也不要查询或引用任何资料。",
		"Please reply directly without searching the knowledge base.",
	} {
		if e.shouldAutoInjectKB(gateMsg(q, nil)) {
			t.Errorf("明确拒绝知识检索的请求仍触发自动注入: %q", q)
		}
	}

	for _, q := range []string{
		"不要猜，请查询知识库",
		"不要凭空回答，请根据文档说明",
		"如何关闭知识库检索功能？",
		"公司年假制度是怎么规定的",
	} {
		if !e.shouldAutoInjectKB(gateMsg(q, nil)) {
			t.Errorf("有效知识检索请求被否定词误杀: %q", q)
		}
	}
}
