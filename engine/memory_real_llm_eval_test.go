package engine

// 真实模型记忆 E2E（hex-test：测已实现的记忆存储/检索/召回，真模型不 mock）。
//
// 三段闭环，单一对话驱动：
//   ① 存储：真 LLM 从对话自动抽取 → 原子化 + 分类 + dedupUpsert 落 FileMemory（增量 3/4）
//   ② 检索：召回内核对 query 命中相关记忆（增量 1/2/4.5，确定性）
//   ③ 召回：**新会话**提问（无对话历史），模型只能靠注入的长期记忆作答（端到端）
//
// 非确定、花真实 token/算力，故 HEXCLAW_REAL_LLM_EVAL=1 门控，读 ~/.hexclaw/hexclaw.yaml。
// 默认本地 qwen3.5:9b；可覆盖跑云端：
//
//	# 本地
//	HEXCLAW_REAL_LLM_EVAL=1 go test ./engine/ -run TestMemoryRealLLM -count=1 -v -timeout 600s
//	# 云端（智谱 glm）
//	HEXCLAW_REAL_LLM_EVAL=1 HEXCLAW_REAL_LLM_PROVIDER="智谱 AI" HEXCLAW_REAL_LLM_MODEL=glm-4.5-air \
//	  go test ./engine/ -run TestMemoryRealLLM -count=1 -v -timeout 600s

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/memory"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

func TestMemoryRealLLM_StoreRetrieveRecall(t *testing.T) {
	if os.Getenv("HEXCLAW_REAL_LLM_EVAL") != "1" {
		t.Skip("set HEXCLAW_REAL_LLM_EVAL=1 to run the real-LLM memory eval (spends tokens / hits Ollama)")
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Skipf("no local hexclaw config: %v", err)
	}
	cfg.Compaction.Enabled = false
	cfg.FileMemory.AutoMemory = "extract" // 本 eval 验证后台抽取路径（inline 默认下需主模型调工具）

	// auto-extract 走 router 的**默认路由**，故经 cfg.LLM.Default + provider.Model 选模型。
	provider := memEvalEnv("HEXCLAW_REAL_LLM_PROVIDER", "Ollama (本地)")
	model := memEvalEnv("HEXCLAW_REAL_LLM_MODEL", "qwen3.5:9b")
	pc, ok := cfg.LLM.Providers[provider]
	if !ok {
		t.Skipf("provider %q not configured in hexclaw.yaml", provider)
	}
	pc.Model = model
	cfg.LLM.Providers[provider] = pc
	cfg.LLM.Default = provider
	// 隔离选中 provider：禁用其它，避免智能路由「偏好本地」回退到未拉取模型（如 gemma4:e4b 404），
	// 从而**确定性地**用选中模型跑 主回复 + auto-extract，得到该模型的真实记忆能力结论。
	for name := range cfg.LLM.Providers {
		p := cfg.LLM.Providers[name]
		en := name == provider
		p.Enabled = &en
		cfg.LLM.Providers[name] = p
	}
	t.Logf("=== 真实模型记忆 E2E（隔离单 provider）：provider=%q model=%q ===", provider, model)

	ctx := context.Background()
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "mem.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	fm, err := memory.New(memory.Options{Enabled: true, Dir: filepath.Join(dir, "memory"), MaxMemory: 200, DailyDays: 0})
	if err != nil {
		t.Fatalf("filemem: %v", err)
	}

	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Skipf("build real provider router: %v", err)
	}
	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	eng.SetFileMemory(fm) // 启用 auto-extract + 记忆注入
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("start engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	// ── ① 存储：真 LLM 从对话抽取记忆 ──────────────────────────────
	m1 := &adapter.Message{
		ID: "mem-real-store", Platform: adapter.PlatformDesktop,
		UserID: "real-eval-user", ChatID: "mem-store-chat",
		Content:   "你好，我叫小明，是一名 Go 后端工程师。我偏好简洁的代码风格。还有很重要的一点：我对花生过敏，请务必记住。",
		Metadata:  map[string]string{},
		Timestamp: time.Now(),
	}
	c1, cancel1 := context.WithTimeout(ctx, 200*time.Second)
	r1, err := eng.Process(c1, m1)
	cancel1()
	if err != nil {
		t.Skipf("turn1 provider unavailable (network/credit/ollama down): %v", err)
	}
	t.Logf("turn1 reply: %s", memEvalTrunc(r1.Content, 160))

	entries := memEvalWaitMemory(fm, 120*time.Second)
	t.Logf("[STORE] auto-extract 落盘 %d 条：", len(entries))
	for _, e := range entries {
		t.Logf("   [%s/%s] %s", e.Type, e.Source, e.Content)
	}
	if len(entries) == 0 {
		t.Errorf("🔴[STORE] 真 LLM 未抽取出任何记忆（auto-extract 链路或模型产出为空）")
		return
	}
	raw := fm.GetMemory()
	allergyStored := strings.Contains(raw, "过敏") || strings.Contains(raw, "花生")
	if allergyStored {
		t.Logf("🟢[STORE] 关键记忆「花生过敏」已抽取存储（共 %d 条原子记忆）", len(entries))
	} else {
		t.Errorf("🔴[STORE] 关键记忆「花生过敏」未被抽取；落盘内容：%q", raw)
	}

	// ── ② 检索：召回内核对 query 命中相关记忆（确定性）──────────────
	block := eng.buildLongTermMemoryBlock(ctx, "", "我能吃花生酱吗")
	t.Logf("[RETRIEVE] 「花生酱」召回块：%s", memEvalTrunc(block, 220))
	if allergyStored {
		if strings.Contains(block, "过敏") || strings.Contains(block, "花生") {
			t.Logf("🟢[RETRIEVE] 「花生酱」query 召回了过敏记忆")
		} else {
			t.Errorf("🔴[RETRIEVE] 已存过敏记忆，但「花生酱」query 未召回；block=%q", block)
		}
	}

	// ── ③ 召回：新会话（无历史）只能靠注入记忆作答 ─────────────────
	m2 := &adapter.Message{
		ID: "mem-real-recall", Platform: adapter.PlatformDesktop,
		UserID: "real-eval-user", ChatID: "mem-recall-chat-NEW", // 新会话：无 turn1 历史
		Content:   "根据你对我的了解，我适合吃花生酱吗？请简短回答并说明原因。",
		Metadata:  map[string]string{},
		Timestamp: time.Now(),
	}
	c2, cancel2 := context.WithTimeout(ctx, 200*time.Second)
	r2, err := eng.Process(c2, m2)
	cancel2()
	if err != nil {
		t.Skipf("turn2 provider unavailable: %v", err)
	}
	t.Logf("[RECALL] turn2 reply: %s", r2.Content)
	if allergyStored {
		if memEvalContainsAny(r2.Content, "过敏", "不适合", "不能", "不宜", "避免", "别吃", "不建议", "不要") {
			t.Logf("🟢[RECALL] 新会话模型据注入记忆正确识别过敏不宜")
		} else {
			t.Errorf("🔴[RECALL] 新会话模型未据注入记忆识别过敏（回答未提示不宜）：%q", r2.Content)
		}
	}
}

// memEvalWaitMemory 轮询等待异步 auto-extract 落盘（至多 max）。
func memEvalWaitMemory(fm *memory.FileMemory, max time.Duration) []memory.MemoryEntry {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if e := fm.ParseEntries(); len(e) > 0 {
			return e
		}
		time.Sleep(2 * time.Second)
	}
	return fm.ParseEntries()
}

func memEvalEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func memEvalTrunc(s string, n int) string {
	r := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

func memEvalContainsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
