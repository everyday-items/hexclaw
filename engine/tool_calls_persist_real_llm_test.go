package engine

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"

	"github.com/hexagon-codes/hexagon"
)

// TestRealLLM_ToolCallsPersistAcrossReload 真模型端到端证明 P1 持久化修复：
// 真 SF 模型走完整 Process（ReAct + 工具）→ SaveAssistantReply 落库 → 从存储重载
// → 重载的 assistant.meta.tool_calls 必须仍在（修复前为空 = 工具卡蒸发）。
//
// 跑法：HEXCLAW_REAL_LLM_EVAL=1 go test ./engine/ -run ToolCallsPersistAcrossReload -v
// 读 ~/.hexclaw/hexclaw.yaml 的 硅基流动 配置（与桌面 app 同一份）。
func TestRealLLM_ToolCallsPersistAcrossReload(t *testing.T) {
	if memEvalEnv("HEXCLAW_REAL_LLM_EVAL", "") != "1" {
		t.Skip("set HEXCLAW_REAL_LLM_EVAL=1 to run real-LLM tool_calls persistence proof (spends tokens)")
	}
	cfg, err := config.Load(memEvalEnv("HEXCLAW_REAL_LLM_CONFIG", ""))
	if err != nil {
		t.Skipf("load config: %v", err)
	}
	cfg.Compaction.Enabled = false
	cfg.LLM.Tools.Enabled = "on"

	provider := memEvalEnv("HEXCLAW_REAL_LLM_PROVIDER", "硅基流动")
	model := memEvalEnv("HEXCLAW_REAL_LLM_MODEL", "Qwen/Qwen3.6-35B-A3B")
	pc, ok := cfg.LLM.Providers[provider]
	if !ok {
		t.Skipf("provider %q not in config (有: %v)", provider, providerNames(cfg))
	}
	pc.Model = model
	cfg.LLM.Providers[provider] = pc
	cfg.LLM.Default = provider
	for name := range cfg.LLM.Providers {
		p := cfg.LLM.Providers[name]
		en := name == provider
		p.Enabled = &en
		cfg.LLM.Providers[name] = p
	}
	t.Logf("=== tool_calls 持久化 真机：provider=%q model=%q ===", provider, model)

	ctx := context.Background()
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "persist.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Skipf("router: %v", err)
	}

	// 注册 cron_task 工具（既有真测证明真模型会可靠调它）→ 产生一次真实 tool_call。
	compiler := cron.NewLLMCompiler(func() (hexagon.Provider, string, error) {
		return router.Route(context.Background())
	})
	sched := cron.NewScheduler(store.DB(), compiler, nil)
	if err := sched.Init(ctx); err != nil {
		t.Fatalf("sched init: %v", err)
	}
	reg := skill.NewRegistry()
	if err := reg.Register(builtin.NewCronTaskSkill(sched, "")); err != nil {
		t.Fatalf("register cron_task: %v", err)
	}
	eng := NewReActEngine(cfg, router, store, reg)
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	eng.SetToolCollector(NewToolCollector(reg, nil, 40))
	eng.SetToolExecutor(NewToolExecutor(reg, nil))

	reply, err := eng.Process(ctx, &adapter.Message{
		Platform: adapter.PlatformDesktop, UserID: "user-persist", ChatID: "chat-persist",
		Content:  "帮我创建一个定时任务：每天早上9点采集百度热搜榜并写入知识库。直接用 cron_task 工具创建，不用问我确认。",
		Metadata: map[string]string{}, Timestamp: time.Now(),
	})
	if err != nil {
		t.Skipf("real provider unavailable, inconclusive: %v", err)
	}
	if len(reply.ToolCalls) == 0 {
		t.Skipf("[EVAL] 真模型本轮未调工具（tool-use 弱）—— 取证条件不满足，非 bug 判定")
	}

	// 取本轮会话 id（Reply 不带 session id，按 user 反查存储）。
	sessions, err := store.ListSessions(ctx, "user-persist", 10, 0)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("反查会话失败: %v (n=%d)", err, len(sessions))
	}
	sessionID := sessions[0].ID
	t.Logf("LIVE tool_calls=%d session=%s", len(reply.ToolCalls), sessionID)

	// ===== 从存储重载（= 切会话/重启的加载路径）=====
	msgs, err := store.ListMessages(ctx, sessionID, 100, 0)
	if err != nil {
		t.Fatalf("reload list messages: %v", err)
	}
	persisted := 0
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		var meta struct {
			ToolCalls []adapter.ToolCall `json:"tool_calls"`
		}
		_ = json.Unmarshal([]byte(m.Metadata), &meta)
		persisted += len(meta.ToolCalls)
	}
	t.Logf("RELOADED 持久化 tool_calls=%d", persisted)

	if persisted == 0 {
		t.Fatalf("P1 未修复：live 有 %d 个 tool_calls，但重载后存储里 0 个 → 工具卡仍会蒸发（该轮走的保存路径未带上 tool_calls）", len(reply.ToolCalls))
	}
	t.Logf("✅ 修复证实：真模型 tool_calls 经 Process 落库，重载后仍在（%d 个）", persisted)
}
