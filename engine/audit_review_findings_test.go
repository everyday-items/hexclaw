package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/memory"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
	"github.com/hexagon-codes/hexclaw/storage"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// 评审findings 实证（RED-first）。范围：单用户桌面（多 Agent）。

// ── G（修复后回归）：主动召回排除当前会话，不再把本会话自己的对话当历史片段重复浮现 ──
// 真实 sqlite store，零 LLM。ctx 带当前 sessionID → Prefetch 据此排除当前会话。
func TestAuditG_ActiveRecallExcludesCurrentSession(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	// 「当前会话」cur 的前一轮对话已落库。
	if err := store.CreateSession(ctx, &storage.Session{ID: "cur", UserID: "u1", Platform: "web", Title: "当前对话"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMessage(ctx, &storage.MessageRecord{
		ID: "m1", SessionID: "cur", Role: "user", Content: "我想把这个服务用蓝绿部署上线", Metadata: "{}",
	}); err != nil {
		t.Fatal(err)
	}

	e := &ReActEngine{}
	e.SetFileMemory(newFileMem(t, 200))
	e.SetActiveRecall(NewActiveRecall(store))

	// 本轮在同一 cur 会话内继续问部署：ctx 带 current sessionID=cur → 应排除本会话自己的话。
	uctx := context.WithValue(skill.WithAuthenticatedUser(ctx, "u1"), ctxKeySessionID, "cur")
	turn := e.buildTurnContext(uctx, map[string]string{}, "", "部署")
	if strings.Contains(turn, "蓝绿部署上线") {
		t.Fatalf("🔴 BUG-G 未修复：主动召回仍把【当前会话】自己的话当历史片段重复浮现:\n%s", turn)
	}

	// 反证：换一个不同的当前会话（other），cur 会话的内容应作为"历史"被正常召回（证明排除是按会话精确的）。
	octx := context.WithValue(skill.WithAuthenticatedUser(ctx, "u1"), ctxKeySessionID, "other")
	turn2 := e.buildTurnContext(octx, map[string]string{}, "", "部署")
	if !strings.Contains(turn2, "蓝绿部署上线") {
		t.Fatalf("跨会话历史应正常召回（只排当前会话），却没召回:\n%s", turn2)
	}
}

// ── 证据 F：minScore=0 无相关性地板——真实 bge-m3 算出低相关事实仍被注入（embedding 已接，地板该补）──
// 环境门控真机：存一条与 query 明显无关的事实，真实向量相关度很低，但 minScore=0 仍把它注入上下文（噪音）。
func TestAuditF_NoRelevanceFloorWithRealEmbedding(t *testing.T) {
	if memEvalEnv("HEXCLAW_REAL_LLM_EVAL", "") != "1" {
		t.Skip("set HEXCLAW_REAL_LLM_EVAL=1 to run real-embedding floor proof")
	}
	cfg, err := config.Load(memEvalEnv("HEXCLAW_REAL_LLM_CONFIG", ""))
	if err != nil {
		t.Skipf("config: %v", err)
	}
	embProvider := memEvalEnv("HEXCLAW_REAL_LLM_PROVIDER", "硅基流动")
	embModel := memEvalEnv("HEXCLAW_REAL_LLM_EMBED_MODEL", "BAAI/bge-m3")
	epc, ok := cfg.LLM.Providers[embProvider]
	if !ok || epc.APIKey == "" {
		t.Skipf("no embed provider key")
	}
	var eopts []hexagon.OpenAIOption
	if epc.BaseURL != "" {
		eopts = append(eopts, hexagon.OpenAIWithBaseURL(epc.BaseURL))
	}
	ai := hexagon.NewOpenAI(epc.APIKey, eopts...)
	dim := hexagon.OpenAIEmbeddingDimension(embModel)
	if dim <= 0 {
		dim = 1024
	}
	emb := hexagon.NewCachedEmbedder(hexagon.NewOpenAIEmbedder(ai,
		hexagon.WithEmbedderModel(embModel), hexagon.WithEmbedderDimension(dim)))

	fm := newFileMem(t, 200)
	mustSave(t, fm, "用户想了解 Kubernetes 的滚动更新策略", "fact") // 与 query 相关
	mustSave(t, fm, "用户养了一只名叫橘座的橘猫", "fact")            // 与 query 明显无关
	e := engineWithFileMem(t, fm)
	e.SetMemoryEmbedder(emb)

	query := "k8s 部署怎么做滚动发布"

	// 先看真实向量相关度：无关事实 cosine 应明显低。
	cands, err := (&memEntrySource{entries: toRecallEntries(fm.ParseEntriesForRole("")), embedder: emb}).
		Candidates(context.Background(), "", "", query, 10)
	if err != nil {
		t.Skipf("embed: %v", err)
	}
	var catRel float64
	for _, c := range cands {
		if strings.Contains(c.Content, "橘猫") {
			catRel = c.VectorScore
		}
	}
	block := e.buildLongTermMemoryBlock(context.Background(), "", query)
	t.Logf("[F] 无关「橘猫」事实真实 cosine=%.3f；注入块:\n%s", catRel, block)

	if strings.Contains(block, "橘猫") {
		t.Fatalf("🔴 BUG-F 未修复：「橘猫」真实 cosine=%.3f（低相关）仍被注入。相关性地板(RecallMinScore)应砍掉它。", catRel)
	}
	if !strings.Contains(block, "Kubernetes") {
		t.Fatalf("相关事实(Kubernetes)不该被地板误砍（漏召）:\n%s", block)
	}
	t.Logf("🟢[F] 相关性地板生效：低相关「橘猫」(cosine=%.3f)被砍，相关「Kubernetes」保留", catRel)
}

// ── 证据 D：inline 自管把"记不记"押在模型 tool-use 上——真实弱本地模型 qwen3.5:9b 静默失忆 ──
// 真机本地：inline 模式 + Ollama qwen3.5:9b（工具调用不稳）。说一句明显该记的「海鲜过敏」，
// 若模型没真调 manage_memory，记忆为空——extract 旧法本会兜住，inline 无兜底 → 静默丢失。
func TestAuditD_InlineSilentFailOnWeakLocalModel(t *testing.T) {
	if memEvalEnv("HEXCLAW_REAL_LOCAL", "") != "1" {
		t.Skip("set HEXCLAW_REAL_LOCAL=1 (需 Ollama qwen3.5:9b) 跑弱模型 inline 静默失忆证明")
	}
	ctx := context.Background()
	yes := true
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"ollama": {BaseURL: "http://localhost:11434/v1", Model: "qwen3.5:9b", APIKey: "ollama", Compatible: "openai", ToolsEnabled: &yes, Enabled: &yes},
	}
	cfg.LLM.Default = "ollama"
	cfg.Compaction.Enabled = false
	cfg.FileMemory.AutoMemory = "inline"

	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	fm, err := memory.New(memory.Options{Enabled: true, Dir: filepath.Join(dir, "mem"), MaxMemory: 200})
	if err != nil {
		t.Fatal(err)
	}
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Skipf("router: %v", err)
	}
	reg := skill.NewRegistry()
	_ = reg.Register(builtin.NewManageMemorySkill(fm))
	eng := NewReActEngine(cfg, router, store, reg)
	eng.SetFileMemory(fm)
	eng.SetToolCollector(NewToolCollector(reg, nil, 0))
	eng.SetToolExecutor(NewToolExecutor(reg, nil))
	if err := eng.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	m := &adapter.Message{
		ID: "d1", Platform: adapter.PlatformDesktop, UserID: "u1", ChatID: "d",
		Content: "我对海鲜严重过敏，麻烦帮我记一下。", Metadata: map[string]string{}, Timestamp: time.Now(),
	}
	c, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	r, err := eng.Process(c, m)
	if err != nil {
		t.Skipf("local model unavailable: %v", err)
	}
	t.Logf("[D] qwen3.5:9b 回复: %s", memEvalTrunc(r.Content, 120))

	// 修复后：inline + 本地 provider → 兜底走后台抽取（异步）。等抽取 goroutine 落盘。
	entries := memEvalWaitMemory(fm, 240*time.Second)
	raw := fm.GetMemory()
	t.Logf("[D] inline+本地兜底 落库 %d 条: %q", len(entries), raw)
	if !strings.Contains(raw, "海鲜") && !strings.Contains(raw, "过敏") {
		t.Fatalf("🔴 BUG-D 未修复：inline + 本地 qwen3.5:9b 未兜底抽取，「海鲜过敏」静默丢失。")
	}
	t.Logf("🟢[D] 修复生效：inline 下本地模型 tool-use 不稳 → 自动兜底后台抽取，「海鲜过敏」未丢失。")
}
