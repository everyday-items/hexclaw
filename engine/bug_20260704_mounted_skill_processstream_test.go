package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// BUG-20260704：会话挂载了 skill（前端 chip「前女友」→ metadata["skills"]）但人设没被引用。
// 现有测试只直接调 buildStreamMessages 验注入；本测试走**真实入口 ProcessStream**，用 capturing
// provider 抓发给模型的完整消息，断言挂载的 persona 正文确实到达模型（system prompt）——
// 覆盖「msg.Metadata → ProcessStream → buildStreamMessages → buildMountedSkillsPrompt」全链路。
func TestBug20260704_MountedSkill_ReachesModelViaProcessStream(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}

	const marker = "你是用户的前女友迪丽热巴，用亲密口吻回应，绝不说自己是AI EXGF_MARK_7788"
	skills := skill.NewRegistry()
	if err := skills.Register(&fakePersonaSkill{name: "前女友", body: marker}); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"test": {Model: "mock-model"}}
	prov := &fastKBChatProvider{}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": prov})
	eng := NewReActEngine(cfg, router, store, skills)
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	ch, err := eng.ProcessStream(context.Background(), &adapter.Message{
		ID: "m-mount", Platform: adapter.PlatformAPI, UserID: "u1", SessionID: "s-mount",
		Content:  "想我了吗？",
		Metadata: map[string]string{"skills": "前女友"},
	})
	if err != nil {
		t.Fatalf("ProcessStream: %v", err)
	}
	for range ch { // drain
	}

	if !prov.sawText("EXGF_MARK_7788") {
		t.Fatalf("BUG-20260704: 挂载的 persona 技能正文未到达模型（ProcessStream 全链路断裂）。"+
			"metadata[\"skills\"]=前女友 但发给模型的消息不含人设 marker。发给模型的消息条数=%d", len(prov.seen))
	}
}
